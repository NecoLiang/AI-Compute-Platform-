package compute

// 托管履约(docs/25, fulfillment_mode=managed)在 compute 侧的最小接入面。
//
// compute 不依赖 fleet: 节点/卡槽归 fleet, 商品/订单归 compute(docs/25 §25.3 表归属)。
// fleet 通过 ManagedGate 注入"能否提交审核"的判定, 并调用 SetManagedStockTx 回写派生库存;
// 未注入(fleet.enabled=false)时托管商品不可提交审核、不可下单, 人工模式行为不变。

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

const (
	FulfillmentManual  = "manual"
	FulfillmentManaged = "managed"
)

// ManagedGate 由 fleet 实现, main 在 fleet.enabled=true 时注入。
type ManagedGate interface {
	// CheckManagedSubmit 托管商品提交审核前校验: 至少一个 verified 节点且 GPU 型号与商品一致。
	// 不满足时返回 40001 的 CodedError, 文案逐项列出不通过原因。
	CheckManagedSubmit(productID int64, gpuModel string) error
	// RefreshManagedStock 按卡槽重算派生库存(审核通过后立即生效, 不等下一轮探活)。
	RefreshManagedStock(productID int64) error
}

// ErrManagedDisabled fleet 未启用时托管商品的统一提示(docs/25 §25.12)。
var ErrManagedDisabled = errcode.New(errcode.ParamInvalid, "托管履约未启用(fleet.enabled=false): 托管商品暂不可提交审核或下单")

// errManagedLeaseNotWired 托管订单需要的租约开通模块(docs/25 S2)尚未接入。
var errManagedLeaseNotWired = errcode.New(errcode.InternalError, "托管履约模块未启用: 租约开通(docs/25 S2)尚未接入, 托管商品暂不可下单")

// SetManagedGate 注入托管判定(fleet)。
func (s *Service) SetManagedGate(g ManagedGate) { s.managedGate = g }

func fulfillmentOrManual(mode string) string {
	if mode == "" {
		return FulfillmentManual
	}
	return mode
}

// initialProductStatus 人工商品创建即进入审核; 托管商品先为草稿,
// 等节点探活通过后由供给方提交审核(docs/25 §25.4.2 ①⑧)。
func initialProductStatus(mode string) string {
	if mode == FulfillmentManaged {
		return "draft"
	}
	return "pending"
}

// checkManagedSubmit 托管商品提交审核/重新提交时的闸门。
func (s *Service) checkManagedSubmit(productID int64, gpuModel string) error {
	if s.managedGate == nil {
		return ErrManagedDisabled
	}
	return s.managedGate.CheckManagedSubmit(productID, strings.TrimSpace(gpuModel))
}

// ManagedProduct 供 fleet 读取商品(只读, 不经 compute repository 之外的路径访问 products)。
// 商品不存在返回 (nil, nil)。
func (s *Service) ManagedProduct(productID int64) (*Product, error) {
	p, err := s.repo.GetProductByID(productID)
	if err != nil && err.Error() == "product not found" { // GetProductByID 的不存在哨兵文案
		return nil, nil
	}
	return p, err
}

// LockManagedProductTx 派生库存重算的串行化锁: 计数与回写之前锁商品行(SELECT … FOR UPDATE)。
// 配合 READ COMMITTED 事务, 拿到锁之后的计数语句能看到上一个持锁者已提交的卡槽变化, 不会丢更新(CR-S1 M3)。
func (s *Service) LockManagedProductTx(tx *sqlx.Tx, productID int64) error {
	var id int64
	err := tx.Get(&id, `SELECT id FROM products WHERE id=? FOR UPDATE`, productID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 商品已不存在: 后续 SetManagedStockTx 为空操作
	}
	return err
}

// SetManagedStockTx 由 fleet 在自己的事务内回写托管商品派生库存:
// stock = 可售空闲卡数; 只在 active↔sold_out 间切换, 不触碰草稿/待审/下架/冻结状态。
// 只作用于 fulfillment_mode='managed' 的行, 人工商品不受影响。
func (s *Service) SetManagedStockTx(tx *sqlx.Tx, productID int64, stock int) error {
	if stock < 0 {
		stock = 0
	}
	if _, err := tx.Exec(`UPDATE products SET stock=?,
		status = CASE WHEN status='active' AND ?<=0 THEN 'sold_out'
		              WHEN status='sold_out' AND ?>0 THEN 'active' ELSE status END
		WHERE id=? AND fulfillment_mode='managed'`, stock, stock, stock, productID); err != nil {
		return err
	}
	return nil
}

// beginManagedAware 托管订单的事务使用 READ COMMITTED: 释放/预留卡槽后在同一事务内重算派生库存,
// 计数必须在拿到商品行锁之后读到最新已提交数据(REPEATABLE READ 的快照可能早于锁, 造成丢更新)。
// 人工订单保持默认隔离级别, 行为不变。fulfillment_mode 下单后不可变, 事务外预读是安全的。
func (s *Service) beginManagedAware(managed bool) (*sqlx.Tx, error) {
	if managed {
		return s.db.BeginTxx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	}
	return s.db.Beginx()
}

func (s *Service) orderIsManaged(orderNo string) (bool, error) {
	var mode string
	err := s.db.Get(&mode, `SELECT fulfillment_mode FROM orders WHERE order_no=?`, orderNo)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return mode == FulfillmentManaged, err
}
