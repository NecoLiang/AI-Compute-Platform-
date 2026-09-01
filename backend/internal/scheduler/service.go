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

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"time"
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
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

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
func (s *Service) Heartbeat(nodeID int64, nodeKey string, availableCards int, gpuUtil, vramUtil *int) error {
	if availableCards < 0 {
		return fmt.Errorf("available_cards 不能为负")
	}
	node, err := s.repo.GetNode(nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("节点不存在")
	}
	if availableCards > node.TotalCards {
		return fmt.Errorf("available_cards 超过节点总卡数 %d", node.TotalCards)
	}
	ok, err := s.repo.RecordHeartbeat(nodeID, hashKey(nodeKey), availableCards, gpuUtil, vramUtil)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("节点密钥不正确")
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
	return s.repo.ListAllNodes(status, page, pageSize)
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
	nodes, err := s.repo.ListNodesByProduct(o.ProductID)
	if err != nil {
		return nil, err
	}
	advice := &ScheduleAdvice{OrderNo: o.OrderNo, ProductID: o.ProductID, NeedCards: o.Quantity, GeneratedAt: time.Now()}
	if len(nodes) == 0 {
		advice.Summary = "该商品未注册算力节点, 无法给出调度建议; 请供应方先注册节点并接入心跳"
		return advice, nil
	}
	for _, n := range nodes {
		beats := 0
		if b, err := s.repo.HeartbeatCount24h(n.ID); err == nil {
			beats = b
		}
		advice.Nodes = append(advice.Nodes, scoreNode(n, o.Quantity, beats))
	}
	sort.SliceStable(advice.Nodes, func(i, j int) bool { return advice.Nodes[i].Score > advice.Nodes[j].Score })
	advice.Summary = summarize(advice.Nodes, o.Quantity)
	return advice, nil
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
