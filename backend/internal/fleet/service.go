package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/modeldeploy"
	"tokenfactory/internal/scheduler"
	"tokenfactory/pkg/config"
	tfcrypto "tokenfactory/pkg/crypto"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

// ProductStore compute 暴露给 fleet 的商品读取与派生库存回写(*compute.Service 实现)。
type ProductStore interface {
	ManagedProduct(productID int64) (*compute.Product, error)
	LockManagedProductTx(tx *sqlx.Tx, productID int64) error
	SetManagedStockTx(tx *sqlx.Tx, productID int64, stock, maxFreePerNode int) error
	// 订单侧(托管开通/失败善后), 全部在 fleet 的事务内调用。
	LockManagedOrderTx(tx *sqlx.Tx, orderID int64) (*compute.Order, error)
	FailManagedOrderTx(tx *sqlx.Tx, orderID int64) (bool, error)
	FailManagedRestartTx(tx *sqlx.Tx, parentID int64) (bool, error)
	BeginManagedProvisioningTx(tx *sqlx.Tx, orderID int64) (*compute.Order, error)
	ActivateManagedOrderTx(tx *sqlx.Tx, orderID int64, selftestAt time.Time, acc compute.ManagedAccess) (*time.Time, error)
	// 网关鉴权与买家访问信息(T-085/T-086)。
	ManagedAccessValue(q sqlx.Queryer, orderID int64) (string, error)
	OrderForAccess(orderNo string) (*compute.Order, error)
	ManagedOrderByID(orderID int64) (*compute.Order, error)
	// 到期(T-087): 托管订单 active → completed(守卫式, 同事务经 ReleaseTx 转 grace)。
	CompleteExpiredManagedOrder(orderNo string) (bool, error)
	HasPendingRestartRenewalTx(tx *sqlx.Tx, parentID int64) (bool, error)
	// 挖矿特征自动冻结(T-088, Q-42): 走与运营冻结相同的订单冻结路径(租约 suspended + 断开会话 + 吊销凭证)。
	FreezeOrderTx(tx *sqlx.Tx, orderNo string) (string, error)
	AttestOrderFreeze(orderNo string)
}

// HealthRefresher scheduler 的商品健康度聚合(*scheduler.Service 实现)。
type HealthRefresher interface {
	RefreshProductHealth(productID, supplierID int64)
}

// Notifier 消息中心(*notification.Service 实现)。
type Notifier interface {
	Record(userID int64, notifType, title, content, link string) error
}

// GatewaySelfTester 经访问网关的自检(docs/25 §25.4.3 P11 / §25.5.1 ⑤)。由 accessgw 模块(T-085)提供;
// 未接入时 P11 与开通自检均判为不通过并写明原因, 不伪造通过。
type GatewaySelfTester interface {
	// SelfTestPort P11: 由网关主机(出口 IP 即节点放行的 IP)对节点临时端口做一次 TCP 连接。
	SelfTestPort(ctx context.Context, nodeID int64, host string, port int) error
	// SelfTestLease 开通 ⑤: 以系统自检身份经网关公网入口登录 provisioning 租约, 执行 nvidia-smi -L 并返回输出。
	SelfTestLease(ctx context.Context, leaseNo string) (string, error)
	// RecheckLease 节点重启对账(T-087): 以系统自检身份经网关登录 active 租约(需主服务登记的一次性复检许可)。
	RecheckLease(ctx context.Context, leaseNo string) (string, error)
}

// Deps fleet 的外部依赖, 全部由 main 注入。
type Deps struct {
	Products ProductStore
	Health   HealthRefresher
	Notifier Notifier
	Gateway  GatewaySelfTester // 可为 nil(S2 未接入)
}

type Service struct {
	repo      *Repository
	products  ProductStore
	health    HealthRefresher
	notifier  Notifier
	gateway   GatewaySelfTester
	rdb       *redis.Client  // 到期提醒去重与周期任务多实例互斥(T-087); nil 时不发到期提醒
	outage    OutageRecorder // 停机认定(T-105); nil 时只记日志
	cfg       config.FleetConfig
	gw        config.AccessGWConfig
	gwEgress  []string
	credKey   []byte
	dialer    *Dialer
	benchSecs int
	sampleN   int
	sampleGap int

	extraExecutors map[string]taskExecutor // probe 之外的任务执行器(T-084 provision 等), 启动前注册
	taskTTL        time.Duration           // 任务锁租约, 0 = taskLeaseTTL(测试可缩短)
	taskBeat       time.Duration           // 心跳续期间隔, 0 = taskHeartbeat

	// modelDeploy 一键部署(docs/27, T-098/T-099); nil = 未接入(AttachModelDeploy 未调用), 租约钩子与周期任务不涉及部署。
	modelDeploy *modeldeploy.Service
}

// NewService credKey 为 security.credential_key 解析后的 32 字节密钥; 为空时涉及管理私钥的操作返回 50000。
func NewService(repo *Repository, deps Deps, cfg config.FleetConfig, gw config.AccessGWConfig, credKey []byte) *Service {
	dialer, err := newDialer(cfg.DenyCIDRs)
	if err != nil {
		// 配置加载阶段(validateFleet)已校验 fleet.deny_cidrs; 走到这里说明调用方绕过了校验。
		// 不降级为"只用内置名单": 拒绝名单是安全边界, 直接中止。
		panic("fleet.deny_cidrs 非法: " + err.Error())
	}
	s := &Service{
		repo: repo, products: deps.Products, health: deps.Health, notifier: deps.Notifier, gateway: deps.Gateway,
		cfg: cfg, gw: gw, gwEgress: gw.EgressIPs, credKey: credKey,
		dialer:    dialer,
		benchSecs: 60, sampleN: 6, sampleGap: 10,
	}
	s.registerExecutor(KindProvision, taskExecutor{timeout: provisionTimeout, run: s.runProvisionTask, onDead: s.onProvisionDead})
	s.registerLeaseExecutors()
	return s
}

// SetGateway 注入网关自检实现(main 在网关客户端就绪后调用; 必须在 RunWorkers/RunPeriodic 之前)。
func (s *Service) SetGateway(g GatewaySelfTester) { s.gateway = g }

var (
	errNodeNotFound   = errcode.New(errcode.NotFound, "节点不存在或不属于当前供应方")
	errKeyUnavailable = errcode.New(errcode.InternalError, "security.credential_key 未配置或非法: 无法生成/解密节点管理密钥")
)

func (s *Service) requireCredKey() error {
	if len(s.credKey) == 0 {
		return errKeyUnavailable
	}
	return nil
}

func wrapKeyErr(err error) error {
	if errors.Is(err, tfcrypto.ErrKeyNotConfigured) {
		return errKeyUnavailable
	}
	return err
}

// ownedNode 锁定并校验节点归属; 非本人或非托管节点一律 40400(不泄露存在性)。
func (s *Service) ownedNode(tx *sqlx.Tx, supplierID, nodeID int64) (*Node, error) {
	n, err := s.repo.lockNode(tx, nodeID)
	if err != nil {
		return nil, err
	}
	if n == nil || n.SupplierID != supplierID {
		return nil, errNodeNotFound
	}
	return n, nil
}

func (s *Service) getOwned(supplierID, nodeID int64) (*Node, error) {
	n, err := s.repo.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	if n == nil || n.SupplierID != supplierID {
		return nil, errNodeNotFound
	}
	return n, nil
}

// inTx 执行事务; fn 返回错误即回滚。隔离级别 READ COMMITTED: 事务内重算派生库存时,
// 拿到商品行锁后的计数语句必须读到最新已提交数据(见 recomputeStockTx)。
func (s *Service) inTx(fn func(tx *sqlx.Tx) error) error {
	tx, err := s.repo.db.BeginTxx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// recomputeStockTx 托管商品派生库存在卡槽/节点状态变化的同一事务内重算(docs/26 §26.5.4)。
//
// 并发正确性(CR-S1 M3): 先锁商品行再计数。同一商品的所有重算由该行锁串行化; 事务为 READ COMMITTED,
// 计数语句在拿到锁之后执行, 能读到上一个持锁者已提交的卡槽变化, 不会以旧快照覆盖(丢更新)。
// 计数是非锁定读, 不等待其他事务未提交的卡槽行锁, 因此不会与"已改卡槽、正等商品锁"的事务互相死锁;
// 那些事务拿到锁后会再计一次, 最终值收敛。调用方事务必须是 READ COMMITTED(fleet inTx、
// compute.beginManagedAware 均满足), REPEATABLE READ 下快照可能早于锁, 不得调用。
func (s *Service) recomputeStockTx(tx *sqlx.Tx, productID int64) error {
	if err := s.products.LockManagedProductTx(tx, productID); err != nil {
		return err
	}
	free, maxNode, err := s.repo.freeSlotStats(tx, productID)
	if err != nil {
		return err
	}
	return s.products.SetManagedStockTx(tx, productID, free, maxNode)
}

func (s *Service) refreshHealth(n *Node) {
	if s.health != nil {
		s.health.RefreshProductHealth(n.ProductID, n.SupplierID)
	}
}

func (s *Service) notify(userID int64, title, content string, nodeID int64) {
	s.notifyLink(userID, title, content, fmt.Sprintf("/console/supplier/nodes/%d", nodeID))
}

func (s *Service) notifyLink(userID int64, title, content, link string) {
	if s.notifier == nil || userID == 0 {
		return
	}
	if err := s.notifier.Record(userID, "system", title, content, link); err != nil {
		slog.Error("发送站内通知失败", "user_id", userID, "link", link, "error", err)
	}
}

// ===== scheduler.ManagedNodes =====

const (
	defaultLeasePortStart = 40000
	defaultLeasePortEnd   = 40099
	maxCardsPerNode       = 64
	maxLeasePortSpan      = 1000
)

// RegisterManagedNode POST /supplier/nodes {mgmt_mode:"ssh"}: 建节点并生成专属 ed25519 管理密钥。
func (s *Service) RegisterManagedNode(supplierID int64, req scheduler.ManagedNodeReq) (any, error) {
	name := strings.TrimSpace(req.NodeName)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return nil, errcode.New(errcode.ParamInvalid, "node_name 必填且不超过 64 字")
	}
	if req.TotalCards <= 0 || req.TotalCards > maxCardsPerNode {
		return nil, errcode.Newf(errcode.ParamInvalid, "total_cards 须在 1-%d 之间", maxCardsPerNode)
	}
	p, err := s.products.ManagedProduct(req.ProductID)
	if err != nil {
		return nil, err
	}
	if p == nil || p.SupplierID != supplierID {
		return nil, errcode.New(errcode.NotFound, "商品不存在或不属于当前供应方")
	}
	if p.FulfillmentMode != compute.FulfillmentManaged {
		return nil, errcode.New(errcode.ParamInvalid, "ssh 托管节点只能挂在平台托管(fulfillment_mode=managed)商品下")
	}
	start, end := req.LeasePortStart, req.LeasePortEnd
	if start == 0 && end == 0 {
		start, end = defaultLeasePortStart, defaultLeasePortEnd
	}
	if start < 1024 || end > 65535 || start > end || end-start+1 > maxLeasePortSpan {
		return nil, errcode.Newf(errcode.ParamInvalid, "租约端口段非法: 须在 1024-65535 内、起止有序且不超过 %d 个端口", maxLeasePortSpan)
	}
	if end-start+1 < req.TotalCards+1 {
		return nil, errcode.New(errcode.ParamInvalid, "租约端口段过小: 至少需要 卡数+1 个端口(含探活自检端口)")
	}
	if req.DiskPerCardGB < 0 || req.DiskPerCardGB > 100000 {
		return nil, errcode.New(errcode.ParamInvalid, "disk_per_card_gb 非法")
	}
	if p.DeliveryMode == "container" && req.DiskPerCardGB == 0 {
		return nil, errcode.New(errcode.ParamInvalid, "分卡容器交付须填写每卡数据盘 disk_per_card_gb(GB)")
	}
	expected, err := parseExpectedFingerprint(req.ExpectedHostFingerprint)
	if err != nil {
		return nil, err
	}
	if err := s.requireCredKey(); err != nil {
		return nil, err
	}
	n := &Node{SupplierID: supplierID, ProductID: req.ProductID, NodeName: name, TotalCards: req.TotalCards,
		LeasePortStart: &start, LeasePortEnd: &end}
	if req.DiskPerCardGB > 0 {
		d := req.DiskPerCardGB
		n.DiskPerCardGB = &d
	}
	var id int64
	err = s.inTx(func(tx *sqlx.Tx) error {
		var err error
		if id, err = s.repo.insertNode(tx, n); err != nil {
			if isDuplicate(err) {
				return errcode.New(errcode.Conflict, "该商品下已存在同名节点")
			}
			return err
		}
		pub, enc, err := generateNodeKey(id, s.credKey)
		if err != nil {
			return wrapKeyErr(err)
		}
		if err := s.repo.setNodeKey(tx, id, pub, enc); err != nil {
			return err
		}
		if expected == "" {
			return nil
		}
		// 预置指纹: 首次连接(引导或探活)即按它校验, 不一致在握手阶段中止, 不发送任何凭证。
		if ok, err := s.repo.preconfirmHostKey(tx, id, expected); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("节点 %d 预置主机指纹未写入", id)
			}
			return err
		}
		return s.repo.audit(tx, supplierID, "node.host_key_preconfirmed", "supplier_node", id,
			nil, map[string]any{"fingerprint": expected, "source": "node_create"}, "")
	})
	if err != nil {
		return nil, err
	}
	return s.repo.GetNode(id)
}

// DeleteManagedNode 存在非 free/withheld 卡槽 → 40900; 否则同一事务删卡槽、释放指纹占用、删节点、重算库存。
func (s *Service) DeleteManagedNode(supplierID, nodeID int64) error {
	return s.inTx(func(tx *sqlx.Tx) error {
		n, err := s.ownedNode(tx, supplierID, nodeID)
		if err != nil {
			return err
		}
		// 空闲/停用卡槽, 以及不承载租约的故障卡槽可随节点删除; 其余(预留/在租/排空/在租故障)→ 40900。
		var busy int
		if err := tx.Get(&busy, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=?
			AND state NOT IN ('free','withheld') AND NOT (state='faulty' AND lease_id IS NULL)`, nodeID); err != nil {
			return err
		}
		if busy > 0 {
			return errcode.New(errcode.Conflict, "节点上有进行中的租约, 不能删除")
		}
		if err := s.repo.deleteIdleSlots(tx, nodeID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE node_tasks SET state='failed', last_error='节点已删除', locked_until=NULL
			WHERE node_id=? AND state IN ('pending','running')`, nodeID); err != nil {
			return err
		}
		ok, err := s.repo.deleteNode(tx, nodeID, supplierID)
		if err != nil {
			return err
		}
		if !ok {
			return errNodeNotFound
		}
		if err := s.repo.audit(tx, supplierID, "node.deleted", "supplier_node", nodeID,
			map[string]any{"node_name": n.NodeName, "verify_status": n.VerifyStatus}, nil, ""); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, n.ProductID)
	})
}

// ManagedExtra 运营列表对托管节点附加的信息。
type ManagedExtra struct {
	LastProbe *ProbeSummary  `json:"last_probe"`
	Slots     map[string]int `json:"slots"`
	SSHHost   *string        `json:"ssh_host"`
	SSHPort   *int           `json:"ssh_port"`
	HostKey   *string        `json:"host_key_fingerprint"`
	KeyPinned bool           `json:"host_key_confirmed"`
}

// ProbeSummary 最近一次探活结论摘要。
type ProbeSummary struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Result    string    `json:"result"`
	Failed    []string  `json:"failed_checks"`
	CreatedAt time.Time `json:"created_at"`
}

func summarize(p ProbeReport) *ProbeSummary {
	sum := &ProbeSummary{ID: p.ID, Kind: p.Kind, Result: p.Result, CreatedAt: p.CreatedAt, Failed: []string{}}
	var checks []Check
	if err := jsonUnmarshal(p.Checks, &checks); err == nil {
		for _, c := range checks {
			if !c.OK && !c.Skipped && c.Required {
				sum.Failed = append(sum.Failed, c.ID+" "+c.Name)
			}
		}
	}
	return sum
}

// ManagedNodeExtras GET /admin/nodes 附加: 最近探活结论、卡槽分布、管理连接与指纹状态。
func (s *Service) ManagedNodeExtras(nodeIDs []int64) (map[int64]any, error) {
	reports, err := s.repo.latestReports(nodeIDs)
	if err != nil {
		return nil, err
	}
	q, args, err := sqlx.In(`SELECT node_id, state, COUNT(*) c FROM gpu_slots WHERE node_id IN (?) GROUP BY node_id, state`, nodeIDs)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		NodeID int64  `db:"node_id"`
		State  string `db:"state"`
		C      int    `db:"c"`
	}
	if err := s.repo.db.Select(&rows, q, args...); err != nil {
		return nil, err
	}
	q, args, err = sqlx.In(`SELECT `+nodeColumns+` FROM supplier_nodes WHERE id IN (?) AND mgmt_mode='ssh'`, nodeIDs)
	if err != nil {
		return nil, err
	}
	var nodes []Node
	if err := s.repo.db.Select(&nodes, q, args...); err != nil {
		return nil, err
	}
	out := map[int64]any{}
	extras := map[int64]*ManagedExtra{}
	for i := range nodes {
		n := &nodes[i]
		x := &ManagedExtra{Slots: map[string]int{}, SSHHost: n.SSHHost, SSHPort: n.SSHPort,
			HostKey: n.HostKeyFingerprint, KeyPinned: n.hostKeyConfirmed()}
		if p, ok := reports[n.ID]; ok {
			x.LastProbe = summarize(p)
		}
		extras[n.ID], out[n.ID] = x, x
	}
	for _, r := range rows {
		if x, ok := extras[r.NodeID]; ok {
			x.Slots[r.State] = r.C
		}
	}
	return out, nil
}

// ===== compute.ManagedGate =====

// CheckManagedSubmit 托管商品提交审核: 至少一个 verified 节点, 且其实测 GPU 型号与商品一致。
func (s *Service) CheckManagedSubmit(productID int64, gpuModel string) error {
	var nodes []Node
	if err := s.repo.db.Select(&nodes, `SELECT `+nodeColumns+` FROM supplier_nodes WHERE product_id=? AND mgmt_mode='ssh' ORDER BY id`, productID); err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errcode.New(errcode.ParamInvalid, "托管商品提交审核前须至少接入一个 ssh 托管节点并通过探活")
	}
	var problems []string
	verified := 0
	for _, n := range nodes {
		if n.VerifyStatus != VerifyVerified {
			problems = append(problems, fmt.Sprintf("节点 %s: 核验状态 %s", n.NodeName, n.VerifyStatus))
			continue
		}
		var inv Inventory
		var raw []byte
		err := s.repo.db.Get(&raw, `SELECT inventory FROM node_probe_reports WHERE node_id=? AND kind='verify' AND result='pass'
			ORDER BY id DESC LIMIT 1`, n.ID)
		if err != nil || jsonUnmarshal(raw, &inv) != nil || len(inv.GPUs) == 0 {
			problems = append(problems, fmt.Sprintf("节点 %s: 缺少通过的探活报告", n.NodeName))
			continue
		}
		ok := true
		for _, g := range inv.GPUs {
			if !ModelMatches(gpuModel, g.Name) {
				problems = append(problems, fmt.Sprintf("节点 %s: GPU%d 实测 %s 与商品型号 %s 不一致", n.NodeName, g.Index, g.Name, gpuModel))
				ok = false
				break
			}
		}
		if ok {
			verified++
		}
	}
	if verified > 0 {
		return nil
	}
	return errcode.New(errcode.ParamInvalid, "托管商品暂不可提交审核: "+strings.Join(problems, "; "))
}

// RefreshManagedStock 审核通过等商品状态变化后立即按卡槽重算库存。
func (s *Service) RefreshManagedStock(productID int64) error {
	return s.inTx(func(tx *sqlx.Tx) error { return s.recomputeStockTx(tx, productID) })
}
