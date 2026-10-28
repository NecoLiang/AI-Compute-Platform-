package scheduler

// 节点探活与容量调度 (docs/23)。
//
// 探活: 节点侧以 node_key 每 30s 上报心跳(在线状态+可用卡数+负载); 平台 sweep 任务
// 把心跳超时(90s)的节点判离线, 并把节点状态聚合为商品健康度:
//   healthy(全部在线且有容量) / degraded(部分在线或无可调度容量) / offline(全部离线) / unknown(未接入探活)
// offline 的商品在下单入口被拦截 —— 探活不是仪表盘, 是真实的售卖闸门。
//
// 调度: 给交付/运营环节输出「节点调度建议」。打分透明可解释, 每一分都有依据:
//   40 健康度(在线状态) + 30 容量适配(best-fit, 优先恰好容纳、减少碎片)
//   + 20 负载(GPU 利用率低者优先) + 10 稳定性(近 24h 心跳在线率)
//   托管商品(docs/26 §26.5.5)的容量与碎片按 gpu_slots 计算, 权重不变; 人工商品沿用节点上报的 available_cards。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"
)

const (
	// heartbeatTTL 心跳超时判离线阈值: 3 个心跳周期(30s)容忍抖动。
	heartbeatTTL  = 90 * time.Second
	sweepInterval = 30 * time.Second
	// expectedBeats24h 24h 理论心跳数(30s 一次), 在线率分母。
	expectedBeats24h = 24 * 3600 / 30
)

// Notifier 消息中心接口, 节点/商品健康变化时通知供应方。nil 表示未装配。
type Notifier interface {
	Record(userID int64, notifType, title, content, link string) error
}

type Service struct {
	repo     *Repository
	notifier Notifier
	managed  ManagedNodes
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// ManagedNodeReq 托管节点(mgmt_mode=ssh)注册参数, 沿用 POST /supplier/nodes(docs/25 §25.10)。
type ManagedNodeReq struct {
	ProductID      int64
	NodeName       string
	TotalCards     int
	DiskPerCardGB  int
	LeasePortStart int
	LeasePortEnd   int
}

// ManagedNodes 托管节点的生命周期由 fleet 负责, main 在 fleet.enabled=true 时注入。
// 未注入时: 注册托管节点 40001, 删除托管节点 50000, 管理端列表不附加探活结论。
type ManagedNodes interface {
	RegisterManagedNode(supplierID int64, req ManagedNodeReq) (any, error)
	DeleteManagedNode(supplierID, nodeID int64) error
	// ManagedNodeExtras 按节点 ID 返回附加信息(最近探活结论等), 供运营列表展示。
	ManagedNodeExtras(nodeIDs []int64) (map[int64]any, error)
}

func (s *Service) SetManagedNodes(m ManagedNodes) { s.managed = m }

// RegisterManagedNode 托管节点注册: 委托 fleet。
func (s *Service) RegisterManagedNode(supplierID int64, req ManagedNodeReq) (any, error) {
	if s.managed == nil {
		return nil, errcode.New(errcode.ParamInvalid, "托管履约未启用(fleet.enabled=false): 暂不支持添加 ssh 托管节点")
	}
	return s.managed.RegisterManagedNode(supplierID, req)
}

// ===== 节点注册与心跳 =====

// RegisterNode 供应方注册节点。返回的 nodeKey 明文仅此一次, 库中只存哈希。
func (s *Service) RegisterNode(supplierID, productID int64, nodeName string, totalCards int) (*Node, string, error) {
	if nodeName == "" || totalCards <= 0 {
		return nil, "", fmt.Errorf("node_name 与 total_cards 必填且为正")
	}
	owner, err := s.repo.ProductSupplierID(productID)
	if err != nil {
		return nil, "", err
	}
	if owner == 0 || owner != supplierID {
		return nil, "", fmt.Errorf("商品不存在或不属于当前供应方")
	}
	mode, err := s.repo.ProductFulfillmentMode(productID)
	if err != nil {
		return nil, "", err
	}
	if mode == "managed" {
		return nil, "", fmt.Errorf("托管商品只能添加 ssh 托管节点(mgmt_mode=ssh)")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	key := "nk-" + hex.EncodeToString(raw)
	n := &Node{SupplierID: supplierID, ProductID: productID, NodeName: nodeName,
		NodeKeyHash: hashKey(key), TotalCards: totalCards}
	if err := s.repo.CreateNode(n); err != nil {
		return nil, "", err
	}
	return n, key, nil
}

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// Heartbeat 节点心跳。身份由 node_key 哈希比对完成, 不走用户 JWT(机器不持有用户态)。
// 心跳成功即触发该商品健康度重算 —— 节点恢复上线能立刻解除商品的下单拦截。
//
// 鉴权先行: 这是公开路由, 密钥校验通过之前不返回任何区分性信息(节点是否存在、
// 总卡数几何都属于供给方产能情报), 统一回「节点或密钥不正确」。
func (s *Service) Heartbeat(nodeID int64, nodeKey string, availableCards int, gpuUtil, vramUtil *int) error {
	errUnauthorized := fmt.Errorf("节点或密钥不正确")
	node, err := s.repo.GetNode(nodeID)
	if err != nil {
		return err
	}
	if node == nil || subtle.ConstantTimeCompare([]byte(hashKey(nodeKey)), []byte(node.NodeKeyHash)) != 1 {
		return errUnauthorized
	}
	if availableCards < 0 {
		return fmt.Errorf("available_cards 不能为负")
	}
	if availableCards > node.TotalCards {
		return fmt.Errorf("available_cards 超过节点总卡数 %d", node.TotalCards)
	}
	ok, err := s.repo.RecordHeartbeat(nodeID, hashKey(nodeKey), availableCards, gpuUtil, vramUtil)
	if err != nil {
		return err
	}
	if !ok {
		return errUnauthorized
	}
	s.refreshProductHealth(node.ProductID, node.SupplierID)
	return nil
}

func (s *Service) ListSupplierNodes(supplierID int64) ([]Node, error) {
	return s.repo.ListNodesBySupplier(supplierID)
}

func (s *Service) DeleteNode(id, supplierID int64) error {
	n, err := s.repo.GetNode(id)
	if err != nil {
		return err
	}
	if n != nil && n.SupplierID == supplierID && n.MgmtMode == "ssh" {
		// 托管节点删除要校验在租卡槽、释放卡 UUID 与主机指纹占用、重算派生库存(docs/25 §25.4.2)。
		if s.managed == nil {
			return errcode.New(errcode.InternalError, "托管履约模块未启用(fleet.enabled=false), 无法删除托管节点")
		}
		if err := s.managed.DeleteManagedNode(supplierID, id); err != nil {
			return err
		}
		s.refreshProductHealth(n.ProductID, n.SupplierID)
		return nil
	}
	ok, err := s.repo.DeleteNode(id, supplierID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("节点不存在或不属于当前供应方")
	}
	if n != nil {
		s.refreshProductHealth(n.ProductID, n.SupplierID)
	}
	return nil
}

func (s *Service) ListAllNodes(status string, page, pageSize int) ([]Node, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	list, total, err := s.repo.ListAllNodes(status, page, pageSize)
	if err != nil || s.managed == nil {
		return list, total, err
	}
	var ids []int64
	for _, n := range list {
		if n.MgmtMode == "ssh" {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return list, total, nil
	}
	extras, err := s.managed.ManagedNodeExtras(ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range list {
		if x, ok := extras[list[i].ID]; ok {
			list[i].Managed = x
		}
	}
	return list, total, nil
}

// ===== 探活 sweep =====

// RunLivenessSweep 探活循环: 心跳超时判离线 → 商品健康度聚合 → 变化即通知供应方。
// 阻塞运行, 以 goroutine 启动; ctx 取消后返回。
func (s *Service) RunLivenessSweep(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	pruneCounter := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce()
			// 心跳流水清理低频执行即可: 每 ~30 分钟一次。
			if pruneCounter++; pruneCounter >= 60 {
				pruneCounter = 0
				if n, err := s.repo.PruneHeartbeats(); err == nil && n > 0 {
					slog.Info("已清理过期心跳流水", "rows", n)
				}
			}
		}
	}
}

func (s *Service) sweepOnce() {
	productIDs, err := s.repo.MarkStaleOffline(heartbeatTTL)
	if err != nil {
		slog.Error("节点离线判定失败", "error", err)
		return
	}
	for _, pid := range productIDs {
		owner, err := s.repo.ProductSupplierID(pid)
		if err != nil {
			slog.Error("读取商品归属失败", "product_id", pid, "error", err)
			continue
		}
		s.refreshProductHealth(pid, owner)
	}
}

// RefreshProductHealth 供 fleet 在托管节点状态变化后触发健康度聚合(口径与心跳节点一致)。
func (s *Service) RefreshProductHealth(productID, supplierID int64) {
	s.refreshProductHealth(productID, supplierID)
}

// refreshProductHealth 聚合健康度; 发生跨越可用性边界的变化时通知供应方。
func (s *Service) refreshProductHealth(productID, supplierID int64) {
	prev, next, err := s.repo.UpdateProductHealth(productID)
	if err != nil {
		slog.Error("商品健康度聚合失败", "product_id", productID, "error", err)
		return
	}
	if prev == next || s.notifier == nil || supplierID == 0 {
		return
	}
	switch {
	case next == "offline":
		slog.Warn("商品节点全部离线, 已拦截下单", "product_id", productID)
		s.notify(supplierID, "节点全部离线",
			fmt.Sprintf("您商品(ID %d)的算力节点已全部离线, 平台已暂停该商品下单。请检查节点与心跳上报。", productID), productID)
	case prev == "offline":
		slog.Info("商品节点恢复, 已解除下单拦截", "product_id", productID, "health", next)
		s.notify(supplierID, "节点已恢复",
			fmt.Sprintf("您商品(ID %d)的算力节点已恢复(%s), 下单拦截已解除。", productID, next), productID)
	}
}

func (s *Service) notify(supplierID int64, title, content string, productID int64) {
	link := fmt.Sprintf("/console/supplier/products/%d", productID)
	if err := s.notifier.Record(supplierID, "system", title, content, link); err != nil {
		slog.Error("发送节点健康通知失败", "supplier_id", supplierID, "error", err)
	}
}

// ===== 调度建议 =====

// NodeAdvice 单节点的调度评分明细。每一项得分都有解释, 供页面直接展示。
type NodeAdvice struct {
	NodeID         int64    `json:"node_id"`
	NodeName       string   `json:"node_name"`
	Status         string   `json:"status"`
	AvailableCards int      `json:"available_cards"`
	TotalCards     int      `json:"total_cards"`
	Score          int      `json:"score"`
	Verdict        string   `json:"verdict"` // recommended / alternative / unavailable
	Reasons        []string `json:"reasons"`
	// CapacitySource 容量口径: heartbeat=节点上报的 available_cards(人工商品); gpu_slots=按卡槽计算(托管商品, docs/26 §26.5.5)。
	CapacitySource string `json:"capacity_source"`
	// MaxGroupFree 托管节点同一拓扑组内最大可用卡数(碎片指标); 人工商品为 null。
	MaxGroupFree *int `json:"max_group_free"`
}

type ScheduleAdvice struct {
	OrderNo     string       `json:"order_no"`
	ProductID   int64        `json:"product_id"`
	NeedCards   int          `json:"need_cards"`
	Nodes       []NodeAdvice `json:"nodes"`
	Summary     string       `json:"summary"`
	GeneratedAt time.Time    `json:"generated_at"`
}

// Advise 为订单生成节点调度建议。requesterSupplierID>0 时校验订单商品归属(供应方视角);
// 传 0 表示运营视角不校验。
func (s *Service) Advise(orderNo string, requesterSupplierID int64) (*ScheduleAdvice, error) {
	o, err := s.repo.GetOrderForAdvice(orderNo)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fmt.Errorf("订单不存在")
	}
	owner, err := s.repo.ProductSupplierID(o.ProductID)
	if err != nil {
		return nil, err
	}
	if requesterSupplierID > 0 && owner != requesterSupplierID {
		return nil, fmt.Errorf("订单不属于当前供应方")
	}
	needCards := o.Quantity
	if needCards <= 0 {
		return nil, fmt.Errorf("订单数量无效, 无法生成调度建议")
	}
	cardsPerUnit, err := compute.CardsPerUnit(o.ProductType, o.CardCount, o.MachineCount)
	if err != nil {
		return nil, fmt.Errorf("%w, 无法生成调度建议", err)
	}
	needCards *= cardsPerUnit

	nodes, err := s.repo.ListNodesByProduct(o.ProductID)
	if err != nil {
		return nil, err
	}
	advice := &ScheduleAdvice{OrderNo: o.OrderNo, ProductID: o.ProductID, NeedCards: needCards, GeneratedAt: time.Now()}
	if len(nodes) == 0 {
		advice.Summary = "该商品未注册算力节点, 无法给出调度建议; 请供应方先注册节点并接入心跳"
		return advice, nil
	}
	mode, err := s.repo.ProductFulfillmentMode(o.ProductID)
	if err != nil {
		return nil, err
	}
	var slots map[int64]nodeSlots
	wholeMachine := false
	if mode == "managed" {
		delivery, leaseID, err := s.repo.ManagedOrderContext(o.OrderNo, o.ProductID)
		if err != nil {
			return nil, err
		}
		groups, err := s.repo.ManagedSlotGroups(o.ProductID, leaseID)
		if err != nil {
			return nil, err
		}
		slots, wholeMachine = groupSlots(groups), delivery == "bare_metal"
	}
	for _, n := range nodes {
		beats := 0
		if b, err := s.repo.HeartbeatCount24h(n.ID); err == nil {
			beats = b
		}
		if slots != nil {
			advice.Nodes = append(advice.Nodes, scoreManagedNode(n, slots[n.ID], needCards, beats, wholeMachine))
			continue
		}
		a := scoreNode(n, needCards, beats)
		a.CapacitySource = capacityHeartbeat
		advice.Nodes = append(advice.Nodes, a)
	}
	sort.SliceStable(advice.Nodes, func(i, j int) bool { return advice.Nodes[i].Score > advice.Nodes[j].Score })
	advice.Summary = summarize(advice.Nodes, needCards)
	return advice, nil
}

const (
	capacityHeartbeat = "heartbeat"
	capacitySlots     = "gpu_slots"
)

// nodeSlots 一台托管节点的卡槽汇总。Available=空闲 + 本订单租约已占(建议给本订单时这些卡可用)。
type nodeSlots struct {
	Total, Free, Own int
	Quota            bool
	Groups           []SlotGroup
}

func (c nodeSlots) available() int { return c.Free + c.Own }

func (c nodeSlots) maxGroupAvailable() int {
	m := 0
	for _, g := range c.Groups {
		m = max(m, g.Free+g.Own)
	}
	return m
}

func groupSlots(groups []SlotGroup) map[int64]nodeSlots {
	out := map[int64]nodeSlots{}
	for _, g := range groups {
		c := out[g.NodeID]
		c.Total += g.Total
		c.Free += g.Free
		c.Own += g.Own
		c.Quota = c.Quota || g.Quota
		c.Groups = append(c.Groups, g)
		out[g.NodeID] = c
	}
	return out
}

// scoreManagedNode 托管商品节点打分(docs/26 §26.5.5): 容量与碎片按 gpu_slots 计算, 打分权重与 scoreNode 相同。
// 可调度条件与下单时的卡槽分配候选一致(verified + schedulable + online; 无磁盘配额或整机交付只能整机出租)。
func scoreManagedNode(n Node, c nodeSlots, needCards, beats24h int, wholeMachine bool) NodeAdvice {
	n.AvailableCards, n.TotalCards = c.available(), c.Total
	maxGroup := c.maxGroupAvailable()
	slotReasons := []string{fmt.Sprintf("卡槽: 可用 %d/共 %d 卡, 拓扑组可用分布 %s, 同组最多 %d 卡", c.available(), c.Total, groupSummary(c.Groups), maxGroup)}
	if c.Own > 0 {
		slotReasons = append(slotReasons, fmt.Sprintf("该订单租约已在本节点预留 %d 张卡", c.Own))
	}
	if needCards > 0 && c.available() >= needCards {
		if maxGroup >= needCards {
			slotReasons = append(slotReasons, fmt.Sprintf("%d 卡可在同一拓扑组内分配", needCards))
		} else {
			slotReasons = append(slotReasons, fmt.Sprintf("%d 卡需跨拓扑组拼接, 卡间通信带宽较低", needCards))
		}
	}
	unavailable := func(reason string) NodeAdvice {
		return NodeAdvice{NodeID: n.ID, NodeName: n.NodeName, Status: n.Status, AvailableCards: n.AvailableCards,
			TotalCards: n.TotalCards, Verdict: "unavailable", Reasons: append([]string{reason}, slotReasons...),
			CapacitySource: capacitySlots, MaxGroupFree: &maxGroup}
	}
	if n.Status != "offline" {
		switch {
		case n.VerifyStatus != "verified":
			return unavailable(fmt.Sprintf("节点未通过验收(%s), 不可调度", n.VerifyStatus))
		case !n.Schedulable:
			return unavailable("节点已暂停调度")
		case n.Status != "online":
			return unavailable("托管节点非在线状态, 不可调度")
		case c.Total == 0:
			return unavailable("节点尚无卡槽记录(未完成接入自检)")
		case wholeMachine && (c.available() != c.Total || needCards != c.Total):
			return unavailable(fmt.Sprintf("整机交付需整台 %d 卡全部空闲且订单卡数等于整机卡数", c.Total))
		case !wholeMachine && !c.Quota && needCards != c.Total:
			return unavailable(fmt.Sprintf("节点不支持磁盘配额, 只能整机(%d 卡)出租", c.Total))
		case !wholeMachine && !c.Quota && c.available() != c.Total:
			return unavailable("节点不支持磁盘配额, 只能整机出租, 当前非全部空闲")
		}
	}
	a := scoreNode(n, needCards, beats24h)
	a.Reasons = append(a.Reasons, slotReasons...)
	a.CapacitySource, a.MaxGroupFree = capacitySlots, &maxGroup
	return a
}

func groupSummary(groups []SlotGroup) string {
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		parts = append(parts, fmt.Sprintf("%s:%d", g.TopoGroup, g.Free+g.Own))
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, " ")
}

// scoreNode 透明打分: 40 健康 + 30 容量适配(best-fit) + 20 负载 + 10 稳定性。
func scoreNode(n Node, needCards, beats24h int) NodeAdvice {
	a := NodeAdvice{NodeID: n.ID, NodeName: n.NodeName, Status: n.Status,
		AvailableCards: n.AvailableCards, TotalCards: n.TotalCards}

	if n.Status == "offline" {
		a.Verdict = "unavailable"
		a.Reasons = append(a.Reasons, "节点离线(心跳超时), 不可调度")
		return a
	}
	if needCards > 0 && n.AvailableCards < needCards {
		a.Verdict = "unavailable"
		a.Reasons = append(a.Reasons, fmt.Sprintf("可用 %d 卡不足订单需求 %d 卡", n.AvailableCards, needCards))
		return a
	}

	if n.Status == "online" {
		a.Score += 40
		a.Reasons = append(a.Reasons, "节点在线, 心跳正常")
	} else { // degraded
		a.Score += 20
		a.Reasons = append(a.Reasons, "节点在线但处于高负载/无余量状态")
	}

	// 容量适配: best-fit —— 需求占可用容量比例越高越贴合, 大节点留给大单, 减少碎片。
	if needCards > 0 && n.AvailableCards > 0 {
		fit := 30 * needCards / n.AvailableCards
		a.Score += fit
		a.Reasons = append(a.Reasons, fmt.Sprintf("容量适配: 需求 %d/可用 %d, 适配得分 %d/30", needCards, n.AvailableCards, fit))
	} else if n.AvailableCards > 0 {
		a.Score += 15
	}

	util := 0
	if n.GPUUtilPct != nil {
		util = *n.GPUUtilPct
	}
	loadScore := 20 * (100 - util) / 100
	a.Score += loadScore
	a.Reasons = append(a.Reasons, fmt.Sprintf("当前 GPU 利用率 %d%%, 负载得分 %d/20", util, loadScore))

	stability := beats24h * 10 / expectedBeats24h
	if stability > 10 {
		stability = 10
	}
	a.Score += stability
	a.Reasons = append(a.Reasons, fmt.Sprintf("近 24h 心跳在线率约 %d%%, 稳定性得分 %d/10", min(100, beats24h*100/expectedBeats24h), stability))

	a.Verdict = "alternative"
	return a
}

func summarize(nodes []NodeAdvice, needCards int) string {
	for i := range nodes {
		if nodes[i].Verdict != "unavailable" {
			nodes[i].Verdict = "recommended"
			return fmt.Sprintf("推荐调度到节点「%s」(得分 %d): 满足 %d 卡需求且综合健康度/容量适配/负载最优",
				nodes[i].NodeName, nodes[i].Score, needCards)
		}
	}
	return "当前无可调度节点: 所有节点离线或容量不足, 建议联系供应方扩容或检查节点状态"
}
