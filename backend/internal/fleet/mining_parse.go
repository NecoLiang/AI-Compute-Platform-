package fleet

// 挖矿特征扫描的输出解析与判定(docs/25 §25.7.3, Q-42, T-088)。纯函数, 不碰数据库与网络。

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"tokenfactory/pkg/config"
	"tokenfactory/pkg/netguard"
)

// MiningProc docker top 的一行(pid, comm, args)。
type MiningProc struct {
	PID  int
	Comm string
	Args string
}

// MiningConn 容器网络命名空间里的一条 TCP 连接(只保留已建立/正在建立的外连)。
type MiningConn struct {
	Local  netip.AddrPort
	Remote netip.AddrPort
	State  string // 01 ESTABLISHED / 02 SYN_SENT
}

// MiningScan miningScanCommand 的解析结果。Running=false 表示容器未运行(本轮不判定)。
type MiningScan struct {
	Running bool
	Procs   []MiningProc
	Conns   []MiningConn
}

// MiningHit 一条命中。Freeze=true 的命中(stratum 参数 / 矿池地址外连)按 Q-42 自动冻结, 其余只告警。
type MiningHit struct {
	Kind   string `json:"kind"` // stratum / pool_addr / process / pool_port
	Detail string `json:"detail"`
	Freeze bool   `json:"freeze"`
}

const (
	MiningHitStratum  = "stratum"
	MiningHitPoolAddr = "pool_addr"
	MiningHitProcess  = "process"
	MiningHitPoolPort = "pool_port"
)

// ParseMiningScan 解析 "@@notrunning" 或 "@@top ... @@tcp ... @@end"。缺 @@end 视为输出不完整(返回错误, 本轮不判定)。
func ParseMiningScan(out string) (MiningScan, error) {
	if strings.Contains(out, "@@notrunning") {
		return MiningScan{}, nil
	}
	sec := sections(out)
	if _, ok := sec["end"]; !ok {
		return MiningScan{}, fmt.Errorf("挖矿扫描输出不完整: %q", truncate(strings.TrimSpace(out), 200))
	}
	top, ok := sec["top"]
	if !ok {
		return MiningScan{}, fmt.Errorf("挖矿扫描输出缺少进程表")
	}
	res := MiningScan{Running: true}
	for _, l := range nonEmptyLines(top) {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue // 表头 "PID COMMAND COMMAND"
		}
		p := MiningProc{PID: pid, Comm: f[1]}
		if len(f) > 2 {
			p.Args = strings.Join(f[2:], " ")
		}
		res.Procs = append(res.Procs, p)
	}
	listen := map[uint16]bool{}
	var conns []MiningConn
	for _, l := range nonEmptyLines(sec["tcp"]) {
		f := strings.Fields(l)
		if len(f) < 4 || !strings.HasSuffix(f[0], ":") {
			continue // 表头
		}
		local, err1 := parseProcNetAddr(f[1])
		remote, err2 := parseProcNetAddr(f[2])
		if err1 != nil || err2 != nil {
			continue
		}
		switch f[3] {
		case "0A": // LISTEN
			listen[local.Port()] = true
		case "01", "02":
			conns = append(conns, MiningConn{Local: local, Remote: remote, State: f[3]})
		}
	}
	// 入站连接(本地端口是容器内监听端口, 如网关 → sshd)的远端端口是对方的临时端口, 不能当外连特征。
	for _, c := range conns {
		if listen[c.Local.Port()] || !c.Remote.Addr().IsValid() || c.Remote.Addr().IsUnspecified() || c.Remote.Addr().IsLoopback() {
			continue
		}
		res.Conns = append(res.Conns, c)
	}
	return res, nil
}

// parseProcNetAddr 解析 /proc/net/tcp{,6} 的 "HEXADDR:HEXPORT"。地址按 32 位字小端存放。
func parseProcNetAddr(s string) (netip.AddrPort, error) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return netip.AddrPort{}, fmt.Errorf("地址格式错误: %q", s)
	}
	raw, err := hex.DecodeString(s[:i])
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, fmt.Errorf("地址格式错误: %q", s)
	}
	port, err := strconv.ParseUint(s[i+1:], 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("端口格式错误: %q", s)
	}
	b := make([]byte, len(raw))
	for w := 0; w < len(raw); w += 4 {
		binary.BigEndian.PutUint32(b[w:], binary.LittleEndian.Uint32(raw[w:]))
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), nil
}

// miningRules 平台维护的特征(config.FleetMining)。
type miningRules struct {
	procs map[string]bool
	ports map[uint16]bool
	pools []netip.Prefix
}

func newMiningRules(c config.FleetMining) (*miningRules, error) {
	r := &miningRules{procs: map[string]bool{}, ports: map[uint16]bool{}}
	for _, p := range c.ProcessNames {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			r.procs[p] = true
		}
	}
	for _, p := range c.PoolPorts {
		if p > 0 && p <= 65535 {
			r.ports[uint16(p)] = true
		}
	}
	for _, a := range c.PoolAddrs {
		pf, err := netguard.ParseCIDROrIP(a)
		if err != nil {
			return nil, fmt.Errorf("fleet.mining.pool_addrs 含非法地址 %q: %w", a, err)
		}
		r.pools = append(r.pools, pf)
	}
	return r, nil
}

// stratumRe 矿工命令行的矿池 URL(stratum+tcp:// / stratum+ssl:// / stratum2+tcp:// ...)。
var stratumRe = regexp.MustCompile(`(?i)stratum[0-9]*\+[a-z0-9]+://\S*`)

// Analyze 按 Q-42 判定: stratum 参数、矿池地址外连 → 冻结; 矿工进程名、矿池常用端口外连 → 只告警。
// 同一特征只记一条(按 kind+detail 去重), 冻结类排在前面。
func (r *miningRules) Analyze(sc MiningScan) []MiningHit {
	var freeze, alert []MiningHit
	seen := map[string]bool{}
	add := func(h MiningHit) {
		k := h.Kind + "\x00" + h.Detail
		if seen[k] {
			return
		}
		seen[k] = true
		if h.Freeze {
			freeze = append(freeze, h)
		} else {
			alert = append(alert, h)
		}
	}
	for _, p := range sc.Procs {
		if m := stratumRe.FindString(p.Args); m != "" {
			add(MiningHit{Kind: MiningHitStratum, Freeze: true, Detail: fmt.Sprintf("pid %d %s: %s", p.PID, p.Comm, truncate(m, 120))})
		}
		if r.procs[strings.ToLower(p.Comm)] {
			add(MiningHit{Kind: MiningHitProcess, Detail: fmt.Sprintf("pid %d 进程名 %s", p.PID, p.Comm)})
		}
	}
	for _, c := range sc.Conns {
		ra := c.Remote.Addr()
		pooled := false
		for _, pf := range r.pools {
			if pf.Contains(ra) {
				pooled = true
				add(MiningHit{Kind: MiningHitPoolAddr, Freeze: true, Detail: "外连矿池地址 " + c.Remote.String()})
				break
			}
		}
		if !pooled && r.ports[c.Remote.Port()] {
			add(MiningHit{Kind: MiningHitPoolPort, Detail: "外连矿池常用端口 " + c.Remote.String()})
		}
	}
	return append(freeze, alert...)
}
