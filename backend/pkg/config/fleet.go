package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	tfcrypto "tokenfactory/pkg/crypto"
	"tokenfactory/pkg/netguard"

	"github.com/spf13/viper"
)

// FleetConfig 托管零租的算力调度(docs/25 §25.12)。
// enabled=false(默认)时 fleet 不接线、托管商品不可提交审核与下单, 人工模式零变化;
// enabled=true 时缺任何必填项启动即失败, 并一次列出全部缺失键, 不做降级。
type FleetConfig struct {
	Enabled             bool
	Workers             int
	ProbeConcurrency    int
	ControllerEgressIPs []string
	BaseImages          []BaseImage
	// ProbeImage 平台探针镜像(ref@sha256), P9/P13 在其中执行; 镜像须含 python3+torch(CUDA), 见 docs/25 §25.12。
	ProbeImage       string
	MinDriverVersion string
	HostReserveCPU   int
	HostReserveMemGB int
	// SelftestKeyFile 主服务经网关做开通自检/P11 时使用的 ed25519 私钥文件(0400)。
	// 网关按其指纹识别"系统自检身份", 只允许登录 provisioning 租约与探活临时路由。
	SelftestKeyFile string
	// DenyCIDRs 追加到内置拒绝名单(pkg/netguard.DefaultDenyCIDRs)的目标地址段, 典型为平台自身 VPC/内网网段、
	// 主服务与网关所在公网段。只能追加, 不能放行内置段。元素为 CIDR 或单个 IP。
	DenyCIDRs []string
	// ModelCacheDir 节点上的模型权重缓存目录(docs/27 §27.4.3), 完整探活读取其所在文件系统容量作为
	// 一键部署可行性 F4 的输入。只允许 /[A-Za-z0-9._/-]、不含 ".."(会直接进入节点命令)。默认 /data/omnis-models。
	ModelCacheDir string
	Grace         FleetGrace
	Outage        FleetOutage
	Probe         FleetProbe
}

type BaseImage struct {
	Name string `mapstructure:"name" json:"name"`
	Ref  string `mapstructure:"ref" json:"ref"`
}

type FleetGrace struct {
	Short   time.Duration // 时/日计费
	Long    time.Duration // 周/月计费
	Nightly time.Duration
}

type FleetOutage struct {
	MinMinutes        int
	RefundRatio       float64
	RefundSingleHours int
	// MaxSupplierMinutes30d 30 天内单节点供给方责任停机超过该分钟数即自动停调度(schedulable=0)。
	MaxSupplierMinutes30d int
	// YeepayPartialRefundConfirmed 易宝"多次部分退款/待分账部分退回"能力书面确认前保持 false:
	// 停机补偿只开放顺延, 退费分支返回 50000(docs/25 §25.7.6)。
	YeepayPartialRefundConfirmed bool
}

type FleetProbe struct {
	MinPerfRatio float64
}

// AccessGWConfig 访问网关(cmd/accessgw)与主服务内部接口(docs/25 §25.6.2)。
type AccessGWConfig struct {
	PublicHost     string
	PublicPort     int
	InternalListen string
	EgressIPs      []string
	UpstreamPubkey string
	HMACSecret     string
	// HostKeyFingerprint 网关自身 SSH 主机密钥指纹(SHA256:...), 主服务经网关自检时据此固定网关身份。
	HostKeyFingerprint string
}

func loadFleet(v *viper.Viper, cfg *Config) error {
	f := FleetConfig{
		Enabled:             v.GetBool("fleet.enabled"),
		Workers:             v.GetInt("fleet.workers"),
		ProbeConcurrency:    v.GetInt("fleet.probe_concurrency"),
		ControllerEgressIPs: cleanList(v.GetStringSlice("fleet.controller_egress_ips")),
		ProbeImage:          strings.TrimSpace(v.GetString("fleet.probe_image")),
		MinDriverVersion:    strings.TrimSpace(v.GetString("fleet.min_driver_version")),
		HostReserveCPU:      v.GetInt("fleet.host_reserve_cpu"),
		HostReserveMemGB:    v.GetInt("fleet.host_reserve_mem_gb"),
		SelftestKeyFile:     strings.TrimSpace(v.GetString("fleet.selftest_key_file")),
		DenyCIDRs:           cleanList(v.GetStringSlice("fleet.deny_cidrs")),
		ModelCacheDir:       strings.TrimSpace(v.GetString("fleet.model_cache_dir")),
		Grace: FleetGrace{
			Short:   v.GetDuration("fleet.grace.short"),
			Long:    v.GetDuration("fleet.grace.long"),
			Nightly: v.GetDuration("fleet.grace.nightly"),
		},
		Outage: FleetOutage{
			MinMinutes:                   v.GetInt("fleet.outage.min_minutes"),
			RefundRatio:                  v.GetFloat64("fleet.outage.refund_ratio"),
			RefundSingleHours:            v.GetInt("fleet.outage.refund_single_hours"),
			MaxSupplierMinutes30d:        v.GetInt("fleet.outage.max_supplier_minutes_30d"),
			YeepayPartialRefundConfirmed: v.GetBool("fleet.outage.yeepay_partial_refund_confirmed"),
		},
		Probe: FleetProbe{MinPerfRatio: v.GetFloat64("fleet.probe.min_perf_ratio")},
	}
	if err := v.UnmarshalKey("fleet.base_images", &f.BaseImages); err != nil {
		return fmt.Errorf("fleet.base_images 格式错误(应为 [{name, ref}] 列表): %w", err)
	}
	gw := AccessGWConfig{
		PublicHost:         strings.TrimSpace(v.GetString("accessgw.public_host")),
		PublicPort:         v.GetInt("accessgw.public_port"),
		InternalListen:     strings.TrimSpace(v.GetString("accessgw.internal_listen")),
		EgressIPs:          cleanList(v.GetStringSlice("accessgw.egress_ips")),
		UpstreamPubkey:     strings.TrimSpace(v.GetString("accessgw.upstream_pubkey")),
		HMACSecret:         v.GetString("accessgw.hmac_secret"),
		HostKeyFingerprint: strings.TrimSpace(v.GetString("accessgw.host_key_fingerprint")),
	}
	applyFleetDefaults(&f, &gw)
	cfg.Fleet = f
	cfg.AccessGW = gw
	if !f.Enabled {
		return nil
	}
	return validateFleet(cfg)
}

func applyFleetDefaults(f *FleetConfig, gw *AccessGWConfig) {
	if f.Workers <= 0 {
		f.Workers = 8
	}
	if f.ProbeConcurrency <= 0 {
		f.ProbeConcurrency = 32
	}
	if f.HostReserveCPU <= 0 {
		f.HostReserveCPU = 2
	}
	if f.HostReserveMemGB <= 0 {
		f.HostReserveMemGB = 8
	}
	if f.Grace.Short <= 0 {
		f.Grace.Short = 24 * time.Hour
	}
	if f.Grace.Long <= 0 {
		f.Grace.Long = 72 * time.Hour
	}
	if f.Grace.Nightly <= 0 {
		f.Grace.Nightly = 24 * time.Hour
	}
	if f.Outage.MinMinutes <= 0 {
		f.Outage.MinMinutes = 5
	}
	if f.Outage.RefundRatio <= 0 {
		f.Outage.RefundRatio = 0.10
	}
	if f.Outage.RefundSingleHours <= 0 {
		f.Outage.RefundSingleHours = 2
	}
	if f.Outage.MaxSupplierMinutes30d <= 0 {
		f.Outage.MaxSupplierMinutes30d = 360
	}
	if f.Probe.MinPerfRatio <= 0 {
		f.Probe.MinPerfRatio = 0.7
	}
	if f.ModelCacheDir == "" {
		f.ModelCacheDir = "/data/omnis-models"
	}
	if gw.PublicPort <= 0 {
		gw.PublicPort = 2222
	}
}

// validateFleet 一次性列出全部缺失/非法项, 避免运维改一项重启一次。
func validateFleet(cfg *Config) error {
	f, gw := cfg.Fleet, cfg.AccessGW
	var missing, invalid []string
	if len(f.ControllerEgressIPs) == 0 {
		missing = append(missing, "fleet.controller_egress_ips")
	}
	for _, ip := range f.ControllerEgressIPs {
		if net.ParseIP(ip) == nil {
			invalid = append(invalid, "fleet.controller_egress_ips 含非法 IP: "+ip)
		}
	}
	for _, c := range f.DenyCIDRs {
		if _, err := netguard.ParseCIDROrIP(c); err != nil {
			invalid = append(invalid, "fleet.deny_cidrs 含非法 CIDR/IP: "+c)
		}
	}
	if len(f.BaseImages) == 0 {
		missing = append(missing, "fleet.base_images")
	}
	for i, img := range f.BaseImages {
		if strings.TrimSpace(img.Name) == "" || !IsDigestRef(img.Ref) {
			invalid = append(invalid, fmt.Sprintf("fleet.base_images[%d] 须有 name 且 ref 按 digest 固定(…@sha256:<64hex>)", i))
		}
	}
	if f.ProbeImage == "" {
		missing = append(missing, "fleet.probe_image")
	} else if !IsDigestRef(f.ProbeImage) {
		invalid = append(invalid, "fleet.probe_image 须按 digest 固定(…@sha256:<64hex>)")
	}
	if f.MinDriverVersion == "" {
		missing = append(missing, "fleet.min_driver_version")
	}
	if f.SelftestKeyFile == "" {
		missing = append(missing, "fleet.selftest_key_file")
	}
	if strings.TrimSpace(cfg.Security.CredentialKey) == "" {
		missing = append(missing, "security.credential_key")
	} else if _, err := tfcrypto.ParseKeyHex(cfg.Security.CredentialKey); err != nil {
		// 节点管理私钥用它加密落库; 非法密钥不能"带病启动"(否则节点接入/探活全部 50000)。
		invalid = append(invalid, "security.credential_key 须为 64 位 hex(32 字节)")
	}
	if gw.PublicHost == "" {
		missing = append(missing, "accessgw.public_host")
	}
	if gw.InternalListen == "" {
		missing = append(missing, "accessgw.internal_listen")
	} else if _, _, err := net.SplitHostPort(gw.InternalListen); err != nil {
		invalid = append(invalid, "accessgw.internal_listen 须为 host:port")
	}
	if len(gw.EgressIPs) == 0 {
		missing = append(missing, "accessgw.egress_ips")
	}
	for _, ip := range gw.EgressIPs {
		if net.ParseIP(ip) == nil {
			invalid = append(invalid, "accessgw.egress_ips 含非法 IP: "+ip)
		}
	}
	if gw.UpstreamPubkey == "" {
		missing = append(missing, "accessgw.upstream_pubkey")
	}
	if gw.HMACSecret == "" {
		missing = append(missing, "accessgw.hmac_secret")
	} else if len(gw.HMACSecret) < 32 {
		invalid = append(invalid, "accessgw.hmac_secret 至少 32 字节")
	}
	if gw.HostKeyFingerprint == "" {
		missing = append(missing, "accessgw.host_key_fingerprint")
	} else if !strings.HasPrefix(gw.HostKeyFingerprint, "SHA256:") {
		invalid = append(invalid, "accessgw.host_key_fingerprint 须为 SHA256:... 格式(ssh-keygen -lf 输出)")
	}
	if f.Probe.MinPerfRatio > 1 {
		invalid = append(invalid, "fleet.probe.min_perf_ratio 须在 (0,1]")
	}
	if !modelCacheDirRe.MatchString(f.ModelCacheDir) || strings.Contains(f.ModelCacheDir, "..") {
		invalid = append(invalid, "fleet.model_cache_dir 须为绝对路径, 仅含字母数字与 ._/-, 不含 ..")
	}
	if len(missing) == 0 && len(invalid) == 0 {
		return nil
	}
	msg := "fleet.enabled=true 但配置不完整"
	if len(missing) > 0 {
		msg += "; 缺少: " + strings.Join(missing, ", ")
	}
	if len(invalid) > 0 {
		msg += "; 非法: " + strings.Join(invalid, "; ")
	}
	return fmt.Errorf("%s", msg)
}

// modelCacheDirRe 与 fleet 包内节点命令的路径白名单(safePathRe)一致。
var modelCacheDirRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]{0,200}$`)

// IsDigestRef 镜像引用必须以 @sha256:<64 hex> 结尾, 防止标签被覆盖后换了镜像内容。
func IsDigestRef(ref string) bool {
	i := strings.LastIndex(ref, "@sha256:")
	if i <= 0 {
		return false
	}
	hex := ref[i+len("@sha256:"):]
	if len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// cleanList 兼容 YAML 列表与环境变量逗号/空格分隔两种写法。
func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
