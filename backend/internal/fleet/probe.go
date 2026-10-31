package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"tokenfactory/internal/compute"

	"golang.org/x/crypto/ssh"
)

// 探活 P1–P13(docs/25 §25.4.3)。所有命令为 commands.go 中的固定模板, 参数只来自平台生成值与数据库并经白名单校验。

var checkNames = map[string]string{
	"P1": "TCP 可达", "P2": "SSH 握手与指纹", "P3": "管理密钥认证", "P4": "GPU 清单", "P5": "声明一致性",
	"P6": "拓扑", "P7": "健康", "P8": "主机资源", "P9": "容器运行时", "P10": "磁盘配额能力",
	"P11": "租约端口: 网关可达", "P12": "租约端口: 未裸露", "P13": "短压测",
}

// 单条命令 60s, 拉镜像 30min(docs/25 §25.11)。
const (
	cmdTimeout  = 60 * time.Second
	pullTimeout = 30 * time.Minute
)

var fullChecks = []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9", "P10", "P11", "P12", "P13"}
var recheckChecks = []string{"P1", "P2", "P3", "P11", "P12"}

// errProbeAborted 任务无法执行(节点已删除/停用/缺凭证/代次已变): 任务置 failed, 不写报告。
// stale=true 表示任务已过时(节点代次已变), 节点状态归新代次的操作所有, 中止时不做任何回写。
type errProbeAborted struct {
	msg   string
	stale bool
}

func (e errProbeAborted) Error() string { return e.msg }

// probeRun 一次探活的上下文与中间结果。
type probeRun struct {
	s        *Service
	n        *Node
	mode     string
	trigger  string
	gen      int // 入队时的节点代次(ProbePayload.Generation)
	product  *compute.Product
	cat      *CatalogEntry
	checks   map[string]*Check
	order    []string
	client   Runner
	fp       string
	inv      Inventory
	gpus     []GPU
	docker   DockerInfo
	freeIdx  map[int]bool // 本次可做侵入项的空闲卡(不含在租/预留/排空)
	await    bool         // 首次连接, 待供给方确认指纹
	mismatch string       // 指纹变化时的观测值
	// topoLinks P6 解析出的两两拓扑(index×index), 探活结论落库后写入卡槽(新卡槽在通过时才建)。
	topoLinks [][]string
}

// ProbeOutcome 任务结果(写入 node_tasks.result)。
type ProbeOutcome struct {
	Mode            string   `json:"mode"`
	Pass            bool     `json:"pass"`
	ReportID        int64    `json:"report_id,omitempty"`
	AwaitingHostKey string   `json:"awaiting_host_key,omitempty"`
	Failed          []string `json:"failed_checks,omitempty"`
	Discarded       bool     `json:"discarded,omitempty"` // 探活期间连接信息被修改, 结论作废
}

func (p *probeRun) set(id string, ok bool, detail string) {
	c := p.checks[id]
	if c == nil {
		return
	}
	c.OK, c.Skipped, c.Detail = ok, false, truncate(detail, 1000)
}

func (p *probeRun) failed() []string {
	var out []string
	for _, id := range p.order {
		c := p.checks[id]
		if c.Required && (!c.OK || c.Skipped) {
			out = append(out, id)
		}
	}
	return out
}

func (p *probeRun) list() []Check {
	out := make([]Check, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, *p.checks[id])
	}
	return out
}

func newProbeRun(s *Service, n *Node, mode string) *probeRun {
	ids := fullChecks
	if mode == ProbePortRecheck {
		ids = recheckChecks
	}
	p := &probeRun{s: s, n: n, mode: mode, checks: map[string]*Check{}, order: ids, freeIdx: map[int]bool{}}
	for _, id := range ids {
		p.checks[id] = &Check{ID: id, Name: checkNames[id], Required: id != "P10", Skipped: true, Detail: "未执行(前置检查未通过)"}
	}
	return p
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runProbe 执行一次探活并落库, 返回结论。基础设施错误(DB 等)返回 error 由 worker 重试。
func (s *Service) runProbe(ctx context.Context, nodeID int64, pl ProbePayload) (*ProbeOutcome, error) {
	mode := pl.Mode
	if mode != ProbePortRecheck {
		mode = ProbeFull
	}
	n, err := s.repo.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	switch {
	case n == nil:
		return nil, errProbeAborted{msg: "节点已删除"}
	case n.VerifyStatus == VerifySuspended:
		return nil, errProbeAborted{msg: "节点已被运营停用"}
	case n.Generation != pl.Generation:
		return nil, errProbeAborted{msg: "节点状态已变化(停用/恢复/重装确认/修改连接信息/确认指纹), 本次探活作废", stale: true}
	case !n.sshReady():
		return nil, errProbeAborted{msg: "节点缺少 SSH 连接信息"}
	case n.MgmtPrivkeyEnc == nil:
		return nil, errProbeAborted{msg: "节点缺少管理密钥"}
	case len(s.credKey) == 0:
		return nil, errProbeAborted{msg: errKeyUnavailable.Error()}
	}
	p := newProbeRun(s, n, mode)
	p.trigger, p.gen = pl.Trigger, pl.Generation
	if mode == ProbeFull {
		if p.product, err = s.products.ManagedProduct(n.ProductID); err != nil {
			return nil, err
		}
		if p.product == nil {
			return nil, errProbeAborted{msg: "节点所属商品不存在"}
		}
		if p.cat, err = s.repo.catalogEntry(p.product.GpuModel); err != nil {
			return nil, err
		}
	}
	if err := p.connect(ctx); err != nil {
		return nil, err
	}
	if p.client != nil {
		defer p.client.Close()
		if !p.await {
			if mode == ProbeFull {
				p.inventory(ctx)
				p.runtime(ctx)
			}
			if p.checks["P3"].OK && (mode == ProbePortRecheck || p.checks["P9"].OK) {
				p.ports(ctx)
			}
			if mode == ProbeFull && p.checks["P9"].OK {
				p.bench(ctx)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		// 服务停机/锁超时导致的中断不是节点的问题: 不写结论, 由 worker 按可重试失败处理。
		return nil, fmt.Errorf("探活被中断: %w", err)
	}
	out, err := s.commitProbe(p)
	if err == nil && out != nil && !out.Discarded && p.topoLinks != nil {
		if err := s.recordTopo(n.ID, p.gen, p.gpus, p.topoLinks, time.Now()); err != nil {
			slog.Error("fleet 记录卡间拓扑失败", "node_id", n.ID, "error", err)
		}
	}
	return out, err
}

// connect P1–P3。指纹未确认(TOFU)时记录观测指纹并在 P3 之后停止, 等供给方确认。
func (p *probeRun) connect(ctx context.Context) error {
	n := p.n
	conn, err := p.s.dialer.TCPCheck(ctx, *n.SSHHost, *n.SSHPort)
	if err != nil {
		// 对供给方只给通用提示: 具体错误(含解析地址、拒绝名单命中)只进日志, 不让探活成为探测内网的回显通道。
		slog.Warn("探活 P1 连接管理端口失败", "node_id", n.ID, "error", err)
		p.set("P1", false, "管理端口不可达")
		return nil
	}
	p.set("P1", true, fmt.Sprintf("%s:%d 可达", *n.SSHHost, *n.SSHPort))
	signer, err := loadNodeSigner(*n.MgmtPrivkeyEnc, p.s.credKey)
	if err != nil {
		_ = conn.Close()
		return errProbeAborted{msg: "节点管理私钥不可用: " + err.Error()}
	}
	pinned := ""
	if n.hostKeyConfirmed() {
		pinned = *n.HostKeyFingerprint
	}
	client, info, err := p.s.dialer.Connect(conn, *n.SSHUser, []ssh.AuthMethod{ssh.PublicKeys(signer)}, pinned)
	p.fp = info.Fingerprint
	if errors.Is(err, ErrHostKeyMismatch) {
		p.mismatch = info.Fingerprint
		p.set("P2", false, fmt.Sprintf("主机指纹 %s 与已确认指纹 %s 不一致(节点转 key_mismatch)", info.Fingerprint, pinned))
		return nil
	}
	if info.Fingerprint == "" {
		slog.Warn("探活 P2 SSH 握手失败", "node_id", n.ID, "error", err)
		p.set("P2", false, "SSH 握手失败")
		return nil
	}
	if pinned == "" {
		p.await = true
		if err := p.s.repo.recordObservedFingerprint(p.s.repo.db, n.ID, p.gen, info.Fingerprint); err != nil {
			if client != nil {
				_ = client.Close()
			}
			return err
		}
		p.set("P2", false, fmt.Sprintf("首次连接, 观测到主机指纹 %s(%s), 待供给方在机器上执行 %s 核对并确认后自动继续",
			info.Fingerprint, info.KeyType, fingerprintCheckCommand))
	} else {
		p.set("P2", true, fmt.Sprintf("指纹 %s 与已确认值一致", info.Fingerprint))
	}
	if err != nil {
		p.set("P3", false, "管理密钥认证失败: 请确认公钥行已写入 "+*n.SSHUser+" 的 ~/.ssh/authorized_keys, 且 from= 中的控制面 IP 未被改动")
		return nil
	}
	p.set("P3", true, "管理密钥登录成功")
	p.client = client
	return nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// run 执行一条固定模板命令(单条默认 60s); 非 0 退出码返回 stderr/stdout 摘要作为错误。
func (p *probeRun) run(ctx context.Context, cmd string) (string, error) {
	return p.runFor(ctx, cmd, cmdTimeout)
}

func (p *probeRun) runFor(ctx context.Context, cmd string, d time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	r, err := p.client.Run(ctx, cmd)
	if err != nil {
		return r.Stdout, err
	}
	if r.ExitCode != 0 {
		msg := strings.TrimSpace(r.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(r.Stdout)
		}
		return r.Stdout, fmt.Errorf("退出码 %d: %s", r.ExitCode, truncate(msg, 300))
	}
	return r.Stdout, nil
}

func sortedKeys(m map[int]string) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
