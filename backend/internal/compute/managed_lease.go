package compute

// 托管订单与租约(fleet)的衔接面(docs/25 §25.3 LeaseHooks, S2 T-082/T-084)。
//
// compute 不 import fleet: fleet 通过 SetLeaseHooks 注入租约侧的事务内钩子, compute 在订单状态
// 流转的同一事务内调用; 反方向 fleet 调用本文件导出的 *Tx 方法推进订单(守卫式, RowsAffected=1)。
// 人工订单(fulfillment_mode=manual)完全不经过这里。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tokenfactory/pkg/crypto"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

// LeaseHooks fleet 注入的租约钩子, 全部在调用方事务内执行; 返回错误即整体回滚。
type LeaseHooks struct {
	// ReserveTx 下单: 建租约(reserved) + 按 docs/26 §26.5.2 预留同机卡槽 + 重算派生库存。
	// 无整机余量返回 40900 CodedError。order.ID 已写入。
	ReserveTx func(tx *sqlx.Tx, order *Order, product *Product) error
	// OnPaidTx 收款确认: 租约 reserved→pending, 入队开通任务(docs/25 §25.5.1)。
	OnPaidTx func(tx *sqlx.Tx, order *Order) error
	// ReleaseTx 订单进入 cancelled/refunded/completed: 释放卡槽、租约迁移、入队 stop/purge。
	// to 为订单的目标状态。已释放过的租约必须幂等返回 nil。
	ReleaseTx func(tx *sqlx.Tx, order *Order, to string) error
	// RenewTx 托管续租衔接(T-088)。nil 时托管订单不可续租(50000 明确提示)。
	RenewTx func(tx *sqlx.Tx, parent, renewal *Order) error
}

// SetLeaseHooks 注入租约钩子(fleet.enabled=true 时由 main 调用)。
func (s *Service) SetLeaseHooks(h LeaseHooks) { s.leaseHooks = h }

func (s *Service) leaseWired() bool {
	return s.leaseHooks.ReserveTx != nil && s.leaseHooks.OnPaidTx != nil && s.leaseHooks.ReleaseTx != nil
}

// errManagedRenewNotWired 托管续租(extend/restart 与租约衔接)属 T-088, 未接入前明确拒绝, 不走人工续租路径。
var errManagedRenewNotWired = errcode.New(errcode.InternalError, "托管订单续租尚未接入(docs/25 §25.7.2, T-088), 暂不可续租")

func (s *Service) requireManagedRenewable(parent *Order) error {
	if parent != nil && parent.FulfillmentMode == FulfillmentManaged && s.leaseHooks.RenewTx == nil {
		return errManagedRenewNotWired
	}
	return nil
}

// releaseHoldTx 订单终态流转时归还占用: 人工订单归还 stock_reserved; 托管订单交给 fleet 释放卡槽与租约
// (docs/26 §26.5.3)。调用方已在同一事务内完成守卫式订单状态迁移, 因此同一订单只会释放一次。
func (s *Service) releaseHoldTx(tx *sqlx.Tx, order *Order, to string) error {
	if order.FulfillmentMode != FulfillmentManaged {
		return s.returnReservedStockTx(tx, order)
	}
	if s.leaseHooks.ReleaseTx == nil {
		return errManagedLeaseNotWired
	}
	return s.leaseHooks.ReleaseTx(tx, order, to)
}

// OnOrderPaidTx 由 payment 在确认收款的同一事务内调用。人工订单无操作。
func (s *Service) OnOrderPaidTx(tx *sqlx.Tx, order *Order) error {
	if order == nil || order.FulfillmentMode != FulfillmentManaged {
		return nil
	}
	if s.leaseHooks.OnPaidTx == nil {
		return errManagedLeaseNotWired
	}
	return s.leaseHooks.OnPaidTx(tx, order)
}

// LockManagedOrderTx fleet 侧事务(开通失败善后等)按锁顺序"订单 → 租约 → 节点 → 卡槽 → 商品"先锁订单行。
// 订单不存在返回 (nil, nil)。
func (s *Service) LockManagedOrderTx(tx *sqlx.Tx, orderID int64) (*Order, error) {
	o, err := s.lockOrderByIDTx(tx, orderID)
	if err != nil || o == nil {
		return o, err
	}
	if o.FulfillmentMode != FulfillmentManaged {
		return nil, fmt.Errorf("订单 %s 不是托管订单", o.OrderNo)
	}
	return o, nil
}

func (s *Service) lockOrderByIDTx(tx *sqlx.Tx, orderID int64) (*Order, error) {
	var o Order
	err := tx.Get(&o, "SELECT "+orderColumns+" FROM orders WHERE id=? FOR UPDATE", orderID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// BeginManagedProvisioningTx 开通步骤 ⓪ 的订单侧: paid → provisioning(守卫式)。
// 返回迁移后的订单(重试时订单已是 provisioning, 原样返回); 订单不在 paid/provisioning 时返回其当前状态,
// 由 fleet 放弃开通(已被取消/冻结/退款)。
func (s *Service) BeginManagedProvisioningTx(tx *sqlx.Tx, orderID int64) (*Order, error) {
	o, err := s.lockOrderByIDTx(tx, orderID)
	if err != nil || o == nil {
		return o, err
	}
	if o.FulfillmentMode != FulfillmentManaged {
		return nil, fmt.Errorf("订单 %s 不是托管订单", o.OrderNo)
	}
	if o.Status == "paid" {
		moved, err := s.repo.TransitionOrderStatusTx(tx, o.OrderNo, []string{"paid"}, "provisioning")
		if err != nil {
			return nil, err
		}
		if !moved {
			return nil, fmt.Errorf("订单 %s 状态已变化", o.OrderNo)
		}
		o.Status = "provisioning"
	}
	return o, nil
}

// ManagedAccess 托管订单交付记录中写给买家的网关连接信息(不含节点 IP/端口)。
type ManagedAccess struct {
	GatewayHost string
	GatewayPort int
	Login       string
}

// ActivateManagedOrderTx 开通步骤 ⑥ 的订单侧: provisioning → active, lease_start=自检通过时刻,
// lease_end 按计费周期换算; 生成交付记录与访问凭证(access_value 即网关登录密码, C-06 沿用),
// 交付记录直接为 delivered(托管无签收环节, Q-41)并写 selftest_passed_at。
// 订单已不在 provisioning(并发取消/冻结/退款)时返回 (nil, nil), 由 fleet 放弃激活。
func (s *Service) ActivateManagedOrderTx(tx *sqlx.Tx, orderID int64, selftestAt time.Time, acc ManagedAccess) (*time.Time, error) {
	if len(s.credentialKey) == 0 {
		return nil, crypto.ErrKeyNotConfigured
	}
	o, err := s.lockOrderByIDTx(tx, orderID)
	if err != nil {
		return nil, err
	}
	if o == nil || o.FulfillmentMode != FulfillmentManaged || o.Status != "provisioning" {
		return nil, nil
	}
	var pricingMode string
	if err := tx.Get(&pricingMode, "SELECT pricing_mode FROM products WHERE id=?", o.ProductID); err != nil {
		return nil, err
	}
	if pricingMode == PricingPerpetual {
		return nil, fmt.Errorf("托管订单不支持买断计费")
	}
	start := selftestAt.Truncate(time.Second)
	end := LeaseEndAt(start, pricingMode, o.Duration)

	info, err := json.Marshal(DeliverInfo{IpAddress: acc.GatewayHost, SshPort: acc.GatewayPort, Username: acc.Login,
		CredentialNote: "平台托管租约: 经访问网关登录; 密码即本订单访问凭证 access_value, 推荐在账户中添加 SSH 公钥登录"})
	if err != nil {
		return nil, err
	}
	credEnc, err := crypto.Encrypt(string(info), s.credentialKey)
	if err != nil {
		return nil, err
	}
	accessKey, err := GenerateAccessKey()
	if err != nil {
		return nil, err
	}
	accessVal, err := GenerateAccessValue()
	if err != nil {
		return nil, err
	}
	accessEnc, err := crypto.Encrypt(accessVal, s.credentialKey)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO order_deliveries (order_id, credential_encrypted, access_key, access_value_encrypted,
		access_status, access_expires_at, selftest_passed_at) VALUES (?,?,?,?,'delivered',?,?)
		ON DUPLICATE KEY UPDATE credential_encrypted=VALUES(credential_encrypted), access_key=VALUES(access_key),
		access_value_encrypted=VALUES(access_value_encrypted), access_status='delivered',
		access_expires_at=VALUES(access_expires_at), selftest_passed_at=VALUES(selftest_passed_at), revoked_at=NULL,
		confirmed_by_buyer=0, buyer_confirmed_at=NULL`, o.ID, credEnc, accessKey, accessEnc, end, start); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`UPDATE orders SET status='active', lease_start_at=?, lease_end_at=?
		WHERE id=? AND fulfillment_mode='managed' AND status='provisioning'`, start, end, o.ID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("订单 %s 状态已变化", o.OrderNo)
	}
	return &end, nil
}

// FailManagedOrderTx 开通彻底失败: paid/provisioning → cancelled, lease_start_at 保持 NULL, 吊销凭证。
// 随后由 payment 的 queueCancelledPaidRefunds 自动全额原路退款(docs/25 §25.5.1), 不新增退款路径。
// 订单已不在 paid/provisioning 时返回 false。
func (s *Service) FailManagedOrderTx(tx *sqlx.Tx, orderID int64) (bool, error) {
	o, err := s.lockOrderByIDTx(tx, orderID)
	if err != nil || o == nil {
		return false, err
	}
	if o.FulfillmentMode != FulfillmentManaged {
		return false, fmt.Errorf("订单 %s 不是托管订单", o.OrderNo)
	}
	res, err := tx.Exec(`UPDATE orders SET status='cancelled', lease_start_at=NULL, lease_end_at=NULL
		WHERE id=? AND fulfillment_mode='managed' AND status IN ('paid','provisioning')`, o.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	return true, s.repo.RevokeAccessByOrderTx(tx, o.ID)
}

// ManagedOrderAccess 托管订单访问信息所需的订单字段(买家本人校验在 fleet 侧完成)。
func (s *Service) OrderForAccess(orderNo string) (*Order, error) {
	return s.repo.GetOrderByNo(orderNo)
}

// ManagedAccessValue 网关密码鉴权: 解密订单访问凭证的 access_value(仅 delivered 且未过期)。
// 返回 ("", nil) 表示当前无可用凭证。明文只在调用栈内比对, 不落日志。
func (s *Service) ManagedAccessValue(q sqlx.Queryer, orderID int64) (string, error) {
	if len(s.credentialKey) == 0 {
		return "", crypto.ErrKeyNotConfigured
	}
	var enc string
	err := sqlx.Get(q, &enc, `SELECT COALESCE(access_value_encrypted,'') FROM order_deliveries
		WHERE order_id=? AND access_status='delivered' AND (access_expires_at IS NULL OR access_expires_at > NOW())`, orderID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && enc == "") {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return crypto.Decrypt(enc, s.credentialKey)
}
