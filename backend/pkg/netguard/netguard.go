// Package netguard 平台主动连接供给方地址(节点管理 SSH、租约端口自检、网关 P11 探活)时的目标地址策略,
// 防止供给方填写的 host 把平台变成访问内网/云元数据/平台 VPC 的跳板(SSRF, docs/25 §25.4.2)。
//
// 策略只有拒绝名单, 没有任何放行开关: 默认名单覆盖回环、RFC1918、CGNAT(含阿里云元数据 100.100.100.200)、
// 链路本地(含 169.254.169.254)、ULA、组播、保留与文档段; 平台自身 VPC 等再经配置 fleet.deny_cidrs 追加。
// 同一策略既用于提交时校验(含域名解析结果), 也用于拨号时 net.Dialer.Control 钩子(防 DNS rebinding)。
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// ErrDenied 目标地址落在拒绝名单内。对供给方只给出通用提示, 具体地址只写日志。
var ErrDenied = errors.New("目标地址不允许: 不能指向回环、内网、链路本地、组播或其他保留地址")

// DefaultDenyCIDRs 内置拒绝名单(不可通过配置移除)。
var DefaultDenyCIDRs = []string{
	"0.0.0.0/8",       // 本网络
	"10.0.0.0/8",      // RFC1918
	"100.64.0.0/10",   // CGNAT, 含阿里云元数据 100.100.100.200
	"127.0.0.0/8",     // 回环
	"169.254.0.0/16",  // 链路本地, 含 169.254.169.254 元数据
	"172.16.0.0/12",   // RFC1918
	"192.0.0.0/24",    // IETF 协议分配
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // 6to4 中继(已废弃)
	"192.168.0.0/16",  // RFC1918
	"198.18.0.0/15",   // 基准测试
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // 组播
	"240.0.0.0/4",     // 保留 + 广播
	"::/128",          // 未指定
	"::1/128",         // 回环
	"64:ff9b::/96",    // NAT64(可映射到任意 IPv4)
	"64:ff9b:1::/48",  // 本地 NAT64
	"100::/64",        // 丢弃前缀
	"2001:db8::/32",   // 文档
	"2002::/16",       // 6to4(内嵌 IPv4)
	"fc00::/7",        // ULA
	"fe80::/10",       // 链路本地
	"fec0::/10",       // 站点本地(已废弃)
	"ff00::/8",        // 组播
}

// Policy 目标地址策略。零值不可用, 须经 New 构造。
type Policy struct {
	deny []netip.Prefix
}

// New 内置名单 + 额外名单(fleet.deny_cidrs; 元素可为 CIDR 或单个 IP)。
func New(extra []string) (*Policy, error) {
	p := &Policy{}
	for _, s := range DefaultDenyCIDRs {
		p.deny = append(p.deny, netip.MustParsePrefix(s))
	}
	for _, s := range extra {
		pf, err := ParseCIDROrIP(s)
		if err != nil {
			return nil, err
		}
		p.deny = append(p.deny, pf)
	}
	return p, nil
}

// ParseCIDROrIP "10.1.0.0/16" 或 "10.1.2.3"(视为 /32 或 /128)。
func ParseCIDROrIP(s string) (netip.Prefix, error) {
	if pf, err := netip.ParsePrefix(s); err == nil {
		return pf.Masked(), nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	return netip.Prefix{}, fmt.Errorf("非法 CIDR/IP: %q", s)
}

// Default 只含内置名单的策略。
func Default() *Policy {
	p, _ := New(nil)
	return p
}

// CheckAddr 地址落在拒绝名单内返回 ErrDenied。IPv4-mapped IPv6(::ffff:a.b.c.d)按 IPv4 判断。
func (p *Policy) CheckAddr(a netip.Addr) error {
	if !a.IsValid() {
		return ErrDenied
	}
	a = a.Unmap().WithZone("")
	for _, pf := range p.deny {
		if pf.Contains(a) {
			return ErrDenied
		}
	}
	return nil
}

// CheckIP net.IP 版本。
func (p *Policy) CheckIP(ip net.IP) error {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return ErrDenied
	}
	return p.CheckAddr(a)
}

// Control 供 net.Dialer.Control 使用: 校验实际要连接的(已解析)地址, 覆盖 DNS rebinding。
func (p *Policy) Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrDenied
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return ErrDenied
	}
	return p.CheckAddr(a)
}

// Resolver 解析域名(net.DefaultResolver.LookupNetIP 的形状)。
type Resolver func(ctx context.Context, network, host string) ([]netip.Addr, error)

// ErrUnresolvable 域名无法解析或没有地址(提交时校验用)。
var ErrUnresolvable = errors.New("域名无法解析")
