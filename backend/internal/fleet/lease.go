package fleet

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"slices"
	"strings"
	"time"
)

// 租约(leases, docs/25 §25.8.2, migration 027)。租约归 fleet; 订单归 compute,
// 二者的状态迁移在同一事务内提交(订单侧走 compute.TransitionOrderStatusTx 或 compute 导出的守卫式方法)。

const (
	LeaseReserved     = "reserved"
	LeasePending      = "pending"
	LeaseProvisioning = "provisioning"
	LeaseActive       = "active"
	LeaseParked       = "parked"
	LeaseSuspended    = "suspended"
	LeaseGrace        = "grace"
	LeasePurging      = "purging"
	LeasePurged       = "purged"
	LeaseFailed       = "failed"
	LeaseCancelled    = "cancelled"
)

// leaseFrom 合法迁移表: 目标状态 ← 允许的来源状态(docs/25 §25.8.2 + S2 实现口径)。
//
// 与设计图的差异(S2 实现口径, 已记入 docs/25):
//   - pending → cancelled: 已支付但尚未开通(未建容器)即被取消/退款, 无现场可清, 直接作废;
//   - provisioning → purging: 开通中被取消/退款, 可能已建出部分容器, 交给 purge 清理;
//   - parked → grace: 包夜租约在窗口外到期(容器已停, 数据卷保留进入宽限);
//   - provisioning → pending: 开通重试耗尽后换节点重新分配一次(T-083, docs/25 §25.5.1);
//   - pending → failed: 重新分配时已无可用节点、或换节点后的开通任务在领取前即被判定失败;
//   - grace → pending(restart 续租, 宽限期内原节点重新开通)、parked/suspended 相关迁移由 T-088/T-095 使用;
//   - purged → pending: 宽限期后 restart 续租复用同一租约行(leases.order_id 唯一), 重新分配节点、全新开通(T-088);
//   - pending → purging: 宽限期后 restart 续租(全新开通)在开通完成前退款, 已建出的环境清除(T-088);
//   - pending/provisioning → grace: 宽限期内 restart 续租在开通完成前失败或退款, 回到宽限期并保留原数据卷,
//     grace_until 沿用续租前的值(期满照常清除, T-088)。
var leaseFrom = map[string][]string{
	LeasePending:      {LeaseReserved, LeaseGrace, LeaseProvisioning, LeasePurged},
	LeaseProvisioning: {LeasePending},
	LeaseActive:       {LeaseProvisioning, LeaseSuspended, LeaseParked},
	LeaseParked:       {LeaseActive},
	LeaseSuspended:    {LeaseActive, LeaseParked},
	LeaseGrace:        {LeaseActive, LeaseSuspended, LeaseParked, LeasePending, LeaseProvisioning},
	LeasePurging:      {LeasePending, LeaseProvisioning, LeaseActive, LeaseParked, LeaseSuspended, LeaseGrace},
	LeasePurged:       {LeasePurging},
	LeaseFailed:       {LeaseProvisioning, LeasePending},
	LeaseCancelled:    {LeaseReserved, LeasePending},
}

// leaseTerminal 终态: 端口占用(port_claim)随之释放。
var leaseTerminal = []string{LeasePurged, LeaseFailed, LeaseCancelled}

// CanTransitionLease 状态机校验(单元测试覆盖合法/非法全组合)。
func CanTransitionLease(from, to string) bool {
	return slices.Contains(leaseFrom[to], from)
}

// Lease leases 一行。
type Lease struct {
	ID                int64      `db:"id" json:"id"`
	LeaseNo           string     `db:"lease_no" json:"lease_no"`
	OrderID           int64      `db:"order_id" json:"order_id"`
	BuyerID           int64      `db:"buyer_id" json:"buyer_id"`
	ProductID         int64      `db:"product_id" json:"product_id"`
	NodeID            int64      `db:"node_id" json:"node_id"`
	Cards             int        `db:"cards" json:"cards"`
	DeliveryKind      string     `db:"delivery_kind" json:"delivery_kind"`
	State             string     `db:"state" json:"state"`
	UpstreamPort      *int       `db:"upstream_port" json:"upstream_port"`
	PortClaim         *int       `db:"port_claim" json:"-"`
	UpstreamHostKeyFP *string    `db:"upstream_host_key_fp" json:"upstream_host_key_fp"`
	ContainerName     *string    `db:"container_name" json:"container_name"`
	VolumeName        *string    `db:"volume_name" json:"volume_name"`
	AccessUntil       *time.Time `db:"access_until" json:"access_until"`
	AccessChangedAt   time.Time  `db:"access_changed_at" json:"access_changed_at"`
	GraceUntil        *time.Time `db:"grace_until" json:"grace_until"`
	SuspendedAt       *time.Time `db:"suspended_at" json:"suspended_at"`
	ProvisionedAt     *time.Time `db:"provisioned_at" json:"provisioned_at"`
	PurgedAt          *time.Time `db:"purged_at" json:"purged_at"`
	FailReason        *string    `db:"fail_reason" json:"fail_reason"`
	ResourceAlert     *string    `db:"resource_alert" json:"resource_alert"`
	OutageRemedy      string     `db:"outage_remedy" json:"outage_remedy"`
	CommitImage       *string    `db:"commit_image" json:"-"`
	CreatedAt         time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time  `db:"updated_at" json:"updated_at"`
}

const leaseColumns = `id, lease_no, order_id, buyer_id, product_id, node_id, cards, delivery_kind, state, upstream_port,
	port_claim, upstream_host_key_fp, container_name, volume_name, access_until, access_changed_at, grace_until,
	suspended_at, provisioned_at, purged_at, fail_reason, resource_alert, outage_remedy, commit_image, created_at, updated_at`

// 登录名 = 租约号 = "l-" + 8 位小写 base32(a-z2-7), 5 字节随机数恰好编码为 8 位。
var leaseNoEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

func newLeaseNo() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "l-" + strings.ToLower(leaseNoEnc.EncodeToString(b)), nil
}

// ValidLeaseNo 校验登录名格式(网关鉴权入口, 不合法直接拒绝不查库)。
func ValidLeaseNo(s string) bool {
	if len(s) != 10 || !strings.HasPrefix(s, "l-") {
		return false
	}
	for _, c := range s[2:] {
		if !(c >= 'a' && c <= 'z') && !(c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

func containerNameOf(leaseNo string) string { return "omnis-" + leaseNo }
func volumeNameOf(leaseNo string) string    { return "omnis-" + leaseNo + "-data" }
func networkNameOf(leaseNo string) string   { return "omnis-" + leaseNo + "-net" }

func errIllegalLease(from, to string) error {
	return fmt.Errorf("租约状态迁移非法: %s → %s", from, to)
}
