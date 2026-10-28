package fleet

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/jmoiron/sqlx"
)

// ===== leases =====

func (r *Repository) insertLease(tx *sqlx.Tx, l *Lease) error {
	for i := 0; i < 5; i++ {
		no, err := newLeaseNo()
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO leases (lease_no, order_id, buyer_id, product_id, node_id, cards, delivery_kind, state)
			VALUES (?,?,?,?,?,?,?,'reserved')`, no, l.OrderID, l.BuyerID, l.ProductID, l.NodeID, l.Cards, l.DeliveryKind)
		if isDuplicate(err) && strings.Contains(err.Error(), "lease_no") {
			continue // 40 bit 随机号碰撞, 换号重试
		}
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		l.ID, l.LeaseNo, l.State = id, no, LeaseReserved
		return nil
	}
	return errors.New("租约号连续碰撞, 请重试")
}

func (r *Repository) getLease(q sqlx.Queryer, id int64) (*Lease, error) {
	return r.getLeaseWhere(q, "id=?", id)
}

func (r *Repository) lockLease(tx *sqlx.Tx, id int64) (*Lease, error) {
	return r.getLeaseWhere(tx, "id=? FOR UPDATE", id)
}

func (r *Repository) lockLeaseByOrder(tx *sqlx.Tx, orderID int64) (*Lease, error) {
	return r.getLeaseWhere(tx, "order_id=? FOR UPDATE", orderID)
}

func (r *Repository) getLeaseByOrder(q sqlx.Queryer, orderID int64) (*Lease, error) {
	return r.getLeaseWhere(q, "order_id=?", orderID)
}

func (r *Repository) getLeaseByNo(q sqlx.Queryer, leaseNo string) (*Lease, error) {
	return r.getLeaseWhere(q, "lease_no=?", leaseNo)
}

func (r *Repository) getLeaseWhere(q sqlx.Queryer, where string, args ...any) (*Lease, error) {
	var l Lease
	err := sqlx.Get(q, &l, `SELECT `+leaseColumns+` FROM leases WHERE `+where, args...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// transitionLease 守卫式迁移: 只有来源状态在迁移表内且当前状态匹配时才更新(RowsAffected=1)。
// 每次迁移都刷新 access_changed_at(网关撤销游标), 进入终态时释放端口占用。
// extraSet 为附加的 "col=?" 片段(逗号开头), extraArgs 为其参数。
func (r *Repository) transitionLease(tx *sqlx.Tx, id int64, to string, extraSet string, extraArgs ...any) (bool, error) {
	from := leaseFrom[to]
	if len(from) == 0 {
		return false, errIllegalLease("*", to)
	}
	set := `state=?, access_changed_at=NOW()`
	if containsStr(leaseTerminal, to) {
		set += `, port_claim=NULL`
	}
	q, args, err := sqlx.In(`UPDATE leases SET `+set+extraSet+` WHERE id=? AND state IN (?)`,
		append(append([]any{to}, extraArgs...), id, from)...)
	if err != nil {
		return false, err
	}
	return affected(tx.Exec(q, args...))
}

// touchAccess 刷新撤销游标(访问条件变化但状态不变时, 例如冻结吊销凭证、续租改 access_until)。
func (r *Repository) touchAccess(q sqlx.Execer, id int64) error {
	_, err := q.Exec(`UPDATE leases SET access_changed_at=NOW() WHERE id=?`, id)
	return err
}

// claimPort 端口在 uk_node_port(node_id, port_claim) 上占用; 冲突返回 false。
func (r *Repository) claimPort(tx *sqlx.Tx, leaseID int64, port int) (bool, error) {
	res, err := tx.Exec(`UPDATE leases SET upstream_port=?, port_claim=? WHERE id=? AND port_claim IS NULL`, port, port, leaseID)
	if isDuplicate(err) {
		return false, nil
	}
	return affected(res, err)
}

// leasedPorts 节点上被租约占用的端口(开通分配与 P11 选端口都要排除)。
func (r *Repository) leasedPorts(q sqlx.Queryer, nodeID int64) (map[int]bool, error) {
	var ports []int
	if err := sqlx.Select(q, &ports, `SELECT port_claim FROM leases WHERE node_id=? AND port_claim IS NOT NULL`, nodeID); err != nil {
		return nil, err
	}
	out := make(map[int]bool, len(ports))
	for _, p := range ports {
		out[p] = true
	}
	return out, nil
}

// leaseSlots 租约当前占用的卡槽(按 gpu_index)。
func (r *Repository) leaseSlots(q sqlx.Queryer, leaseID int64) ([]Slot, error) {
	var list []Slot
	err := sqlx.Select(q, &list, `SELECT id, node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group, state, lease_id,
		state_changed_at FROM gpu_slots WHERE lease_id=? ORDER BY gpu_index`, leaseID)
	return list, err
}

// moveLeaseSlots 守卫式批量迁移本租约卡槽: from → to; 迁到 free 时解除租约绑定。返回影响行数。
func (r *Repository) moveLeaseSlots(tx *sqlx.Tx, leaseID int64, from []string, to string) (int64, error) {
	set := `state=?, state_changed_at=NOW()`
	if to == SlotFree {
		set += `, lease_id=NULL`
	}
	q, args, err := sqlx.In(`UPDATE gpu_slots SET `+set+` WHERE lease_id=? AND state IN (?)`, to, leaseID, from)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LeaseFilter 运营租约检索。
type LeaseFilter struct {
	State    string
	NodeID   int64
	BuyerID  int64
	Page     int
	PageSize int
}

func (r *Repository) listLeases(f LeaseFilter) ([]Lease, int64, error) {
	where, args := []string{"1=1"}, []any{}
	if f.State != "" {
		where, args = append(where, "state=?"), append(args, f.State)
	}
	if f.NodeID > 0 {
		where, args = append(where, "node_id=?"), append(args, f.NodeID)
	}
	if f.BuyerID > 0 {
		where, args = append(where, "buyer_id=?"), append(args, f.BuyerID)
	}
	w := " WHERE " + strings.Join(where, " AND ")
	var total int64
	if err := r.db.Get(&total, `SELECT COUNT(*) FROM leases`+w, args...); err != nil {
		return nil, 0, err
	}
	var list []Lease
	err := r.db.Select(&list, `SELECT `+leaseColumns+` FROM leases`+w+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, f.PageSize, (f.Page-1)*f.PageSize)...)
	return list, total, err
}

// setResourceAlert 在租卡异常写入/清空 leases.resource_alert(停机认定与补偿属 T-105)。返回是否发生变化。
func (r *Repository) setResourceAlert(q sqlx.Execer, leaseID int64, alert string) (bool, error) {
	if alert == "" {
		return affected(q.Exec(`UPDATE leases SET resource_alert=NULL WHERE id=? AND resource_alert IS NOT NULL`, leaseID))
	}
	alert = truncate(alert, 127) // truncate 超长时追加省略号, 留 1 字符
	return affected(q.Exec(`UPDATE leases SET resource_alert=? WHERE id=? AND NOT (resource_alert <=> ?)`, alert, leaseID, alert))
}
