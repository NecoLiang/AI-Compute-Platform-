package scheduler

import (
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"
)

// Node 供应方算力节点。node_key 是节点心跳的身份凭证, 库中只存 SHA-256 哈希,
// 明文仅在注册时返回一次(与访问凭证 C-06 同一纪律)。
type Node struct {
	ID              int64      `db:"id" json:"id"`
	SupplierID      int64      `db:"supplier_id" json:"supplier_id"`
	ProductID       int64      `db:"product_id" json:"product_id"`
	NodeName        string     `db:"node_name" json:"node_name"`
	NodeKeyHash     string     `db:"node_key_hash" json:"-"` // ssh 模式为空(库中 NULL)
	Status          string     `db:"status" json:"status"`   // online/degraded/offline
	TotalCards      int        `db:"total_cards" json:"total_cards"`
	AvailableCards  int        `db:"available_cards" json:"available_cards"`
	GPUUtilPct      *int       `db:"gpu_util_pct" json:"gpu_util_pct"`
	VRAMUtilPct     *int       `db:"vram_util_pct" json:"vram_util_pct"`
	LastHeartbeatAt *time.Time `db:"last_heartbeat_at" json:"last_heartbeat_at"`
	// 托管列(migration 026, 由 fleet 写): push=docs/23 心跳节点, ssh=平台 SSH 管理(docs/25)。
	MgmtMode     string `db:"mgmt_mode" json:"mgmt_mode"`
	VerifyStatus string `db:"verify_status" json:"verify_status"`
	Schedulable  bool   `db:"schedulable" json:"schedulable"`
	// Managed 托管节点的附加信息(最近探活结论等), 由 fleet 填充; push 节点为空。
	Managed any `db:"-" json:"managed,omitempty"`
	// SupplierName 供给方公司名, 仅 ListAllNodes(运营视角)填充; 供给方自查列表不带。
	SupplierName string    `db:"supplier_name" json:"supplier_name,omitempty"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

const nodeColumns = "id, supplier_id, product_id, node_name, COALESCE(node_key_hash,'') AS node_key_hash, status, total_cards, available_cards, gpu_util_pct, vram_util_pct, last_heartbeat_at, mgmt_mode, verify_status, schedulable, created_at, updated_at"

type Repository struct{ db *sqlx.DB }

func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (r *Repository) CreateNode(n *Node) error {
	res, err := r.db.Exec(
		"INSERT INTO supplier_nodes (supplier_id, product_id, node_name, node_key_hash, total_cards, available_cards) VALUES (?,?,?,?,?,0)",
		n.SupplierID, n.ProductID, n.NodeName, n.NodeKeyHash, n.TotalCards)
	if err != nil {
		return err
	}
	n.ID, _ = res.LastInsertId()
	return nil
}

func (r *Repository) GetNode(id int64) (*Node, error) {
	var n Node
	err := r.db.Get(&n, "SELECT "+nodeColumns+" FROM supplier_nodes WHERE id=?", id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (r *Repository) ListNodesBySupplier(supplierID int64) ([]Node, error) {
	var list []Node
	err := r.db.Select(&list, "SELECT "+nodeColumns+" FROM supplier_nodes WHERE supplier_id=? ORDER BY id DESC", supplierID)
	return list, err
}

func (r *Repository) ListNodesByProduct(productID int64) ([]Node, error) {
	var list []Node
	err := r.db.Select(&list, "SELECT "+nodeColumns+" FROM supplier_nodes WHERE product_id=? ORDER BY id ASC", productID)
	return list, err
}

func (r *Repository) ListAllNodes(status string, page, pageSize int) ([]Node, int64, error) {
	where, args := "WHERE 1=1", []any{}
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM supplier_nodes "+where, args...); err != nil {
		return nil, 0, err
	}
	var list []Node
	args = append(args, pageSize, (page-1)*pageSize)
	err := r.db.Select(&list, "SELECT "+nodeColumns+`,
		COALESCE((SELECT e.name FROM enterprises e WHERE e.user_id=supplier_nodes.supplier_id),'') AS supplier_name
		FROM supplier_nodes `+where+" ORDER BY id DESC LIMIT ? OFFSET ?", args...)
	return list, total, err
}

func (r *Repository) DeleteNode(id, supplierID int64) (bool, error) {
	res, err := r.db.Exec("DELETE FROM supplier_nodes WHERE id=? AND supplier_id=?", id, supplierID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RecordHeartbeat 心跳落库: 条件校验哈希, 密钥不对不更新任何东西。
// degraded 判定在 SQL 内联完成: 心跳是新鲜的, 但已无可调度容量或负载打满。
func (r *Repository) RecordHeartbeat(nodeID int64, keyHash string, availableCards int, gpuUtil, vramUtil *int) (bool, error) {
	res, err := r.db.Exec(`UPDATE supplier_nodes
		SET available_cards=?, gpu_util_pct=?, vram_util_pct=?, last_heartbeat_at=NOW(),
		    status=IF(? <= 0 OR COALESCE(?,0) >= 95, 'degraded', 'online')
		WHERE id=? AND node_key_hash=?`,
		availableCards, gpuUtil, vramUtil, availableCards, gpuUtil, nodeID, keyHash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// RowsAffected=0 也可能是所有值恰好没变化; 用存在性校验区分「密钥错误」。
		var exists int
		if err := r.db.Get(&exists, "SELECT COUNT(*) FROM supplier_nodes WHERE id=? AND node_key_hash=?", nodeID, keyHash); err != nil {
			return false, err
		}
		if exists == 0 {
			return false, nil
		}
		// 值未变化时也要刷新心跳时间, 单独补一刀。
		_, err = r.db.Exec("UPDATE supplier_nodes SET last_heartbeat_at=NOW() WHERE id=? AND node_key_hash=?", nodeID, keyHash)
		if err != nil {
			return false, err
		}
	}
	_, err = r.db.Exec("INSERT INTO node_heartbeats (node_id, available_cards, gpu_util_pct, vram_util_pct) VALUES (?,?,?,?)",
		nodeID, availableCards, gpuUtil, vramUtil)
	return true, err
}

// MarkStaleOffline 把心跳超时的节点置为离线, 返回受影响的 product_id 列表(去重)。
// 托管节点(mgmt_mode='ssh')不推心跳, 在线状态只由 fleet 的 SSH 探活判定(docs/25 §25.4.4),
// 两条 SQL 都必须排除, 否则会与 fleet 判定来回抖动。
func (r *Repository) MarkStaleOffline(ttl time.Duration) ([]int64, error) {
	var productIDs []int64
	err := r.db.Select(&productIDs, `SELECT DISTINCT product_id FROM supplier_nodes
		WHERE status<>'offline' AND mgmt_mode<>'ssh' AND (last_heartbeat_at IS NULL OR last_heartbeat_at < NOW() - INTERVAL ? SECOND)`,
		int(ttl.Seconds()))
	if err != nil {
		return nil, err
	}
	_, err = r.db.Exec(`UPDATE supplier_nodes SET status='offline'
		WHERE status<>'offline' AND mgmt_mode<>'ssh' AND (last_heartbeat_at IS NULL OR last_heartbeat_at < NOW() - INTERVAL ? SECOND)`,
		int(ttl.Seconds()))
	return productIDs, err
}

// UpdateProductHealth 把节点状态聚合为商品健康度。返回 (变更前, 变更后, 是否变化)。
func (r *Repository) UpdateProductHealth(productID int64) (string, string, error) {
	var prev string
	if err := r.db.Get(&prev, "SELECT health FROM products WHERE id=?", productID); err != nil {
		if err == sql.ErrNoRows {
			return "", "", nil
		}
		return "", "", err
	}
	var agg struct {
		Total  int `db:"total"`
		Online int `db:"online"`
		Usable int `db:"usable"` // online 且有可调度容量
	}
	// SUM 在零行时是 NULL, 必须 COALESCE —— 否则删除最后一个节点后聚合报错, 健康度卡死。
	err := r.db.Get(&agg, `SELECT COUNT(*) AS total,
		COALESCE(SUM(status IN ('online','degraded')),0) AS online,
		COALESCE(SUM(status='online' AND available_cards>0),0) AS usable
		FROM supplier_nodes WHERE product_id=?`, productID)
	if err != nil {
		return "", "", err
	}
	next := "unknown" // 未注册节点的商品不参与探活联动, 不影响既有业务
	switch {
	case agg.Total == 0:
	case agg.Online == 0:
		next = "offline"
	case agg.Usable > 0 && agg.Online == agg.Total:
		next = "healthy"
	default:
		next = "degraded"
	}
	if next != prev {
		if _, err := r.db.Exec("UPDATE products SET health=? WHERE id=?", next, productID); err != nil {
			return prev, next, err
		}
	}
	return prev, next, nil
}

// ProductFulfillmentMode 商品履约模式(manual/managed); 商品不存在返回空串。
func (r *Repository) ProductFulfillmentMode(productID int64) (string, error) {
	var mode string
	err := r.db.Get(&mode, "SELECT fulfillment_mode FROM products WHERE id=?", productID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return mode, err
}

// SlotGroup 托管节点一个拓扑组的卡槽计数(docs/26 §26.5.5)。Own=已由本订单租约预留/占用的卡。
type SlotGroup struct {
	NodeID    int64  `db:"node_id"`
	TopoGroup string `db:"topo_group"`
	Quota     bool   `db:"quota"`
	Total     int    `db:"total"`
	Free      int    `db:"free"`
	Own       int    `db:"own"`
}

// ManagedSlotGroups 托管商品各节点按拓扑组的卡槽计数。leaseID=0 表示订单尚无租约(Own 恒为 0)。
// 只读快照, 不加锁; 真正的分配以下单事务内的卡槽预留为准。
func (r *Repository) ManagedSlotGroups(productID, leaseID int64) ([]SlotGroup, error) {
	var list []SlotGroup
	err := r.db.Select(&list, `SELECT s.node_id, s.topo_group, MAX(COALESCE(n.disk_quota_supported,0))=1 AS quota,
		COUNT(*) AS total,
		COALESCE(SUM(s.state='free'),0) AS free,
		COALESCE(SUM(s.state IN ('reserved','leased') AND s.lease_id=?),0) AS own
		FROM gpu_slots s JOIN supplier_nodes n ON n.id=s.node_id
		WHERE n.product_id=? GROUP BY s.node_id, s.topo_group ORDER BY s.node_id, s.topo_group`, leaseID, productID)
	return list, err
}

// ManagedOrderContext 托管订单的交付方式与租约(无租约时 leaseID=0)。
func (r *Repository) ManagedOrderContext(orderNo string, productID int64) (delivery string, leaseID int64, err error) {
	if err = r.db.Get(&delivery, "SELECT delivery_mode FROM products WHERE id=?", productID); err != nil {
		return "", 0, err
	}
	err = r.db.Get(&leaseID, `SELECT l.id FROM leases l JOIN orders o ON o.id=l.order_id WHERE o.order_no=?`, orderNo)
	if err == sql.ErrNoRows {
		return delivery, 0, nil
	}
	return delivery, leaseID, err
}

// ProductSupplierID 商品归属校验用。
func (r *Repository) ProductSupplierID(productID int64) (int64, error) {
	var sid int64
	err := r.db.Get(&sid, "SELECT supplier_id FROM products WHERE id=?", productID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return sid, err
}

// OrderForAdvice 调度建议所需的订单摘要。
type OrderForAdvice struct {
	ProductType  string `db:"product_type"`
	CardCount    int    `db:"card_count"`
	MachineCount int    `db:"machine_count"`
	OrderNo      string `db:"order_no"`
	ProductID    int64  `db:"product_id"`
	Quantity     int    `db:"quantity"`
	Status       string `db:"status"`
}

func (r *Repository) GetOrderForAdvice(orderNo string) (*OrderForAdvice, error) {
	var o OrderForAdvice
	err := r.db.Get(&o, `SELECT o.order_no, o.product_id, o.quantity, o.status, p.product_type, COALESCE(p.card_count,0) AS card_count, COALESCE(p.machine_count,0) AS machine_count FROM orders o JOIN products p ON p.id=o.product_id WHERE o.order_no=?`, orderNo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// HeartbeatCount24h 近 24h 心跳条数, 用于在线率估算。
func (r *Repository) HeartbeatCount24h(nodeID int64) (int, error) {
	var n int
	err := r.db.Get(&n, "SELECT COUNT(*) FROM node_heartbeats WHERE node_id=? AND created_at > NOW() - INTERVAL 24 HOUR", nodeID)
	return n, err
}

// PruneHeartbeats 清理 7 天前的心跳流水, 防表膨胀。
func (r *Repository) PruneHeartbeats() (int64, error) {
	res, err := r.db.Exec("DELETE FROM node_heartbeats WHERE created_at < NOW() - INTERVAL 7 DAY LIMIT 10000")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
