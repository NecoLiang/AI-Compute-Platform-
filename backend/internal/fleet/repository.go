package fleet

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// Repository fleet 自有表(gpu_slots / node_probe_reports / node_tasks)与 supplier_nodes 托管列的 SQL。
// 状态迁移一律守卫式 UPDATE, 由调用方检查 RowsAffected。
type Repository struct{ db *sqlx.DB }

func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (r *Repository) DB() *sqlx.DB { return r.db }

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// affectedOrMatched 守卫 UPDATE 的写入值可能与现值完全相同(重复保存、同一秒内再次置 NOW()),
// 而 MySQL 默认(未开 clientFoundRows)对"命中但未改变"的行返回 RowsAffected=0。
// 调用方已在本事务内 SELECT ... FOR UPDATE 锁住该行, 因此用同一守卫条件复查是否命中是可靠的。
func affectedOrMatched(tx *sqlx.Tx, res sql.Result, err error, where string, args ...any) (bool, error) {
	ok, err := affected(res, err)
	if err != nil || ok {
		return ok, err
	}
	var n int
	if err := tx.Get(&n, `SELECT COUNT(*) FROM supplier_nodes WHERE `+where, args...); err != nil {
		return false, err
	}
	return n == 1, nil
}

// ===== supplier_nodes =====

func (r *Repository) getNode(q sqlx.Queryer, id int64) (*Node, error) {
	var n Node
	err := sqlx.Get(q, &n, `SELECT `+nodeColumns+` FROM supplier_nodes WHERE id=? AND mgmt_mode='ssh'`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &n, err
}

// GetNode 托管节点(非 ssh 节点返回 nil)。
func (r *Repository) GetNode(id int64) (*Node, error) { return r.getNode(r.db, id) }

func (r *Repository) lockNode(tx *sqlx.Tx, id int64) (*Node, error) {
	var n Node
	err := tx.Get(&n, `SELECT `+nodeColumns+` FROM supplier_nodes WHERE id=? AND mgmt_mode='ssh' FOR UPDATE`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &n, err
}

func (r *Repository) insertNode(tx *sqlx.Tx, n *Node) (int64, error) {
	res, err := tx.Exec(`INSERT INTO supplier_nodes
		(supplier_id, product_id, node_name, node_key_hash, status, total_cards, available_cards, mgmt_mode,
		 verify_status, schedulable, disk_per_card_gb, lease_port_start, lease_port_end)
		VALUES (?,?,?,NULL,'offline',?,0,'ssh','unverified',1,?,?,?)`,
		n.SupplierID, n.ProductID, n.NodeName, n.TotalCards, n.DiskPerCardGB, n.LeasePortStart, n.LeasePortEnd)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) setNodeKey(tx *sqlx.Tx, id int64, pub, encPriv string) error {
	ok, err := affected(tx.Exec(`UPDATE supplier_nodes SET mgmt_pubkey=?, mgmt_privkey_encrypted=?
		WHERE id=? AND mgmt_mode='ssh' AND mgmt_pubkey IS NULL`, pub, encPriv, id))
	if err == nil && !ok {
		err = fmt.Errorf("节点 %d 管理密钥已存在", id)
	}
	return err
}

// updateSSH 修改连接信息: 节点回到 unverified、代次 +1(进行中的探活结论作废); host/port 变化时清除指纹钉死(换了机器必须重新确认)。
func (r *Repository) updateSSH(tx *sqlx.Tx, id int64, host string, port int, user string, resetFP bool) (bool, error) {
	// 重装闸门未解除(T-089)时保持 needs_reimage: 改连接信息不能绕过重装确认。
	q := `UPDATE supplier_nodes SET ssh_host=?, ssh_port=?, ssh_user=?,
		verify_status=CASE WHEN reimage_required_at IS NULL THEN 'unverified' ELSE 'needs_reimage' END,
		host_fp_claim=NULL, verified_at=NULL, generation=generation+1`
	if resetFP {
		q += `, host_key_fingerprint=NULL, host_key_confirmed_at=NULL`
	}
	const where = `id=? AND mgmt_mode='ssh' AND verify_status<>'suspended'`
	res, err := tx.Exec(q+` WHERE `+where, host, port, user, id)
	return affectedOrMatched(tx, res, err, where+` AND ssh_host=? AND ssh_port=? AND ssh_user=?`, id, host, port, user)
}

// recordObservedFingerprint TOFU: 首次连接记录观测指纹, 待供给方确认。只写回同一代次(连接信息未改、未停用/重装)。
func (r *Repository) recordObservedFingerprint(q sqlx.Execer, id int64, gen int, fp string) error {
	_, err := q.Exec(`UPDATE supplier_nodes SET host_key_fingerprint=?
		WHERE id=? AND mgmt_mode='ssh' AND host_key_confirmed_at IS NULL AND generation=? AND verify_status<>'suspended'`,
		fp, id, gen)
	return err
}

// preconfirmHostKey 供给方提交的预期指纹(expected_host_fingerprint)直接钉死: 之后的首次连接按它校验, 不走 TOFU。
// 调用方须在同一事务内已锁住/刚写入节点行, 并已把代次 +1(改连接信息)或节点尚未有任何探活(新建)。
func (r *Repository) preconfirmHostKey(tx *sqlx.Tx, id int64, fp string) (bool, error) {
	const where = `id=? AND mgmt_mode='ssh' AND verify_status<>'suspended'`
	res, err := tx.Exec(`UPDATE supplier_nodes SET host_key_fingerprint=?, host_key_confirmed_at=NOW(), host_fp_claim=NULL
		WHERE `+where, fp, id)
	return affectedOrMatched(tx, res, err, where+` AND host_key_fingerprint=? AND host_key_confirmed_at IS NOT NULL`, id, fp)
}

// confirmHostKey 钉死指纹并把代次 +1: 指纹确认前已在跑的(TOFU)探活结论作废, 由随后入队的完整探活接手。
func (r *Repository) confirmHostKey(tx *sqlx.Tx, id int64, fp string) (bool, error) {
	return affected(tx.Exec(`UPDATE supplier_nodes SET host_key_confirmed_at=NOW(), generation=generation+1
		WHERE id=? AND mgmt_mode='ssh' AND host_key_fingerprint=? AND host_key_confirmed_at IS NULL`, id, fp))
}

// markKeyMismatch 钉死指纹与观测不一致: 停止新分配, 新指纹记为待确认, 释放指纹占用。只写回同一代次的观测。
// 整机重装闸门在时(供给方已重装、收尾任务连到新系统)保持 needs_reimage: 只能经重装确认(提交新指纹)回到可售路径。
func (r *Repository) markKeyMismatch(tx *sqlx.Tx, id int64, gen int, pinned, observed string) (bool, error) {
	return affected(tx.Exec(`UPDATE supplier_nodes SET
		verify_status=CASE WHEN reimage_required_at IS NULL THEN 'key_mismatch' ELSE 'needs_reimage' END,
		host_fp_claim=NULL, status='offline',
		host_key_fingerprint=?, host_key_confirmed_at=NULL
		WHERE id=? AND mgmt_mode='ssh' AND host_key_fingerprint=? AND host_key_confirmed_at IS NOT NULL
		  AND verify_status<>'suspended' AND generation=?`, observed, id, pinned, gen))
}

func (r *Repository) setVerifyStatus(tx *sqlx.Tx, id int64, from []string, to string) (bool, error) {
	q, args, err := sqlx.In(`UPDATE supplier_nodes SET verify_status=? WHERE id=? AND mgmt_mode='ssh' AND verify_status IN (?)`,
		to, id, from)
	if err != nil {
		return false, err
	}
	return affected(tx.Exec(q, args...))
}

func (r *Repository) setSchedulable(id, supplierID int64, v bool) (bool, error) {
	return affected(r.db.Exec(`UPDATE supplier_nodes SET schedulable=? WHERE id=? AND supplier_id=? AND mgmt_mode='ssh' AND schedulable<>?`,
		v, id, supplierID, v))
}

// connGuard 探活结论只写回连接信息未被修改过的节点(防止旧探活覆盖新 SSH 信息的结论)。
type connGuard struct {
	Host string
	Port int
	User string
}

func guardOf(n *Node) connGuard {
	g := connGuard{}
	if n.SSHHost != nil {
		g.Host = *n.SSHHost
	}
	if n.SSHPort != nil {
		g.Port = *n.SSHPort
	}
	if n.SSHUser != nil {
		g.User = *n.SSHUser
	}
	return g
}

// Inventory 探活通过后写回节点的主机清单。
type Inventory struct {
	CPUCores      int            `json:"cpu_cores"`
	MemGB         int            `json:"mem_gb"`
	DiskFreeGB    int            `json:"disk_free_gb"`
	DiskPath      string         `json:"disk_path"`
	QuotaOK       bool           `json:"disk_quota_supported"`
	DriverVersion string         `json:"driver_version"`
	CUDAVersion   string         `json:"cuda_version"`
	OS            string         `json:"os"`
	Kernel        string         `json:"kernel"`
	DockerVersion string         `json:"docker_version"`
	GPUs          []GPU          `json:"gpus"`
	TopoGroups    map[int]string `json:"topo_groups"`
	Faulty        map[int]string `json:"faulty,omitempty"` // gpu_index → 原因
}

// markVerified 完整探活通过: 只允许 verifying(本次入队的完整探活)与 verified(周期补跑)→ verified, 且须同一代次、
// 连接信息与钉死指纹未变。failed/needs_reimage/key_mismatch/suspended 必须先经供给方/运营操作回到 verifying。
func (r *Repository) markVerified(tx *sqlx.Tx, id int64, g connGuard, gen int, fp string, inv *Inventory) (bool, error) {
	// T-089 重装闸门: 整机租约结束未确认重装(reimage_required_at 非空)不得写回; 指纹仍是租期内的旧指纹
	// (reimage_prev_fp, 买家曾持有 root 可读取主机私钥)亦不得写回。
	const where = `id=? AND mgmt_mode='ssh' AND ssh_host=? AND ssh_port=? AND ssh_user=?
		  AND host_key_fingerprint=? AND host_key_confirmed_at IS NOT NULL AND generation=?
		  AND verify_status IN ('verifying','verified')
		  AND reimage_required_at IS NULL AND (reimage_prev_fp IS NULL OR reimage_prev_fp<>host_key_fingerprint)`
	res, err := tx.Exec(`UPDATE supplier_nodes SET verify_status='verified', verified_at=NOW(), host_fp_claim=?, reimage_prev_fp=NULL,
		cpu_cores=?, mem_gb=?, disk_free_gb=?, disk_quota_supported=?, driver_version=?, cuda_version=?,
		status='online', last_heartbeat_at=NOW(), last_probe_ok_at=NOW(), probe_fail_count=0
		WHERE `+where,
		fp, inv.CPUCores, inv.MemGB, inv.DiskFreeGB, inv.QuotaOK, inv.DriverVersion, nullStr(inv.CUDAVersion),
		id, g.Host, g.Port, g.User, fp, gen)
	// 命中但值未变(同一秒内重复通过)仍视为成功; 复查时额外要求结果已是 verified/online。
	return affectedOrMatched(tx, res, err, where+` AND verify_status='verified' AND status='online'`,
		id, g.Host, g.Port, g.User, fp, gen)
}

// markFailed 完整探活失败: unverified/verifying/verified → failed 并释放指纹占用;
// key_mismatch / needs_reimage 保持(失败不改变"待确认"语义), suspended 不动; 代次不一致(结论已过时)不动。
// 返回是否写回(false = 结论作废)。
func (r *Repository) markFailed(tx *sqlx.Tx, id int64, g connGuard, gen int) (bool, error) {
	// 失败节点不再接受周期轻量探活, 同时置 offline, 避免健康度聚合仍按"在线"计。
	res, err := tx.Exec(`UPDATE supplier_nodes SET host_fp_claim=NULL, status='offline',
		verify_status = CASE WHEN verify_status IN ('unverified','verifying','verified','failed') THEN 'failed' ELSE verify_status END
		WHERE id=? AND mgmt_mode='ssh' AND ssh_host=? AND ssh_port=? AND ssh_user=? AND verify_status<>'suspended' AND generation=?`,
		id, g.Host, g.Port, g.User, gen)
	return affectedOrMatched(tx, res, err, `id=? AND mgmt_mode='ssh' AND ssh_host=? AND ssh_port=? AND ssh_user=?
		AND verify_status<>'suspended' AND generation=?`, id, g.Host, g.Port, g.User, gen)
}

// markAwaitingFingerprint 首次连接尚未确认指纹: verifying → unverified, 等供给方确认后自动继续(同一代次才写回)。
func (r *Repository) markAwaitingFingerprint(tx *sqlx.Tx, id int64, gen int) error {
	_, err := tx.Exec(`UPDATE supplier_nodes SET verify_status='unverified'
		WHERE id=? AND mgmt_mode='ssh' AND verify_status IN ('verifying','failed') AND generation=?`, id, gen)
	return err
}

// abandonVerifying 本代次的完整探活未能执行(中止/死信): verifying → failed, 避免节点永远停在 verifying。
// exceptTask 为本次结束的任务; 节点上还有其他未结束的 probe 时不回退。
func (r *Repository) abandonVerifying(id int64, gen int, exceptTask int64) (bool, error) {
	return affected(r.db.Exec(`UPDATE supplier_nodes SET verify_status='failed', host_fp_claim=NULL, status='offline'
		WHERE id=? AND mgmt_mode='ssh' AND verify_status='verifying' AND generation=?
		  AND NOT EXISTS (SELECT 1 FROM node_tasks WHERE node_id=? AND kind='probe' AND state IN ('pending','running') AND id<>?)`,
		id, gen, id, exceptTask))
}

// suspend 运营停用: 代次 +1, 进行中的探活结论一律作废。
func (r *Repository) suspend(tx *sqlx.Tx, id int64) (bool, error) {
	return affected(tx.Exec(`UPDATE supplier_nodes SET verify_status='suspended', host_fp_claim=NULL, generation=generation+1
		WHERE id=? AND mgmt_mode='ssh' AND verify_status<>'suspended'`, id))
}

// resume 运营恢复: 代次 +1(停用前入队/运行的探活不能复用, 也不能把恢复后的节点直接改回 verified)。
func (r *Repository) resume(tx *sqlx.Tx, id int64) (bool, error) {
	// 停用期间整机租约结束(重装闸门已置, T-089): 恢复到 needs_reimage, 须供给方重装确认后才能再探活上架。
	return affected(tx.Exec(`UPDATE supplier_nodes SET
		verify_status=CASE WHEN reimage_required_at IS NULL THEN 'unverified' ELSE 'needs_reimage' END,
		verified_at=NULL, generation=generation+1
		WHERE id=? AND mgmt_mode='ssh' AND verify_status='suspended'`, id))
}

// markNeedsReimage 整机租约(买家曾持有 root)结束: 置重装闸门并记下租期内的钉死指纹, 节点 → needs_reimage
// (运营停用中的节点保持 suspended, 恢复时落到 needs_reimage); 代次 +1, 释放指纹占用。幂等(闸门已置不重复记)。
// 调用方须在同一事务内已锁住节点行。
func (r *Repository) markNeedsReimage(tx *sqlx.Tx, id int64) error {
	_, err := tx.Exec(`UPDATE supplier_nodes SET
		reimage_prev_fp=CASE WHEN reimage_required_at IS NULL THEN host_key_fingerprint ELSE reimage_prev_fp END,
		reimage_required_at=COALESCE(reimage_required_at, NOW()),
		verify_status=CASE WHEN verify_status='suspended' THEN 'suspended' ELSE 'needs_reimage' END,
		host_fp_claim=NULL, verified_at=NULL, generation=generation+1
		WHERE id=? AND mgmt_mode='ssh'`, id)
	return err
}

func (r *Repository) deleteNode(tx *sqlx.Tx, id, supplierID int64) (bool, error) {
	if _, err := tx.Exec(`UPDATE supplier_nodes SET host_fp_claim=NULL WHERE id=? AND supplier_id=? AND mgmt_mode='ssh'`, id, supplierID); err != nil {
		return false, err
	}
	return affected(tx.Exec(`DELETE FROM supplier_nodes WHERE id=? AND supplier_id=? AND mgmt_mode='ssh'`, id, supplierID))
}

func (r *Repository) fingerprintClaimedBy(q sqlx.Queryer, fp string, exceptID int64) (int64, error) {
	var id int64
	err := sqlx.Get(q, &id, `SELECT id FROM supplier_nodes WHERE host_fp_claim=? AND id<>? LIMIT 1`, fp, exceptID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ===== 周期探活 =====

// listVerifiedNodes 周期探活对象: verified 的托管节点(含 offline, 恢复也靠探活发现)。
func (r *Repository) listVerifiedNodes() ([]Node, error) {
	var list []Node
	err := r.db.Select(&list, `SELECT `+nodeColumns+` FROM supplier_nodes
		WHERE mgmt_mode='ssh' AND verify_status='verified' ORDER BY id`)
	return list, err
}

// recordLightOK 轻量探活成功: 在线、心跳时间、可用卡数, 同时写 node_heartbeats(docs/23 口径)。
// 返回之前的 status, 供调用方判断是否跨越在线边界。
func (r *Repository) recordLightOK(tx *sqlx.Tx, id int64, available int, gpuUtil, vramUtil int) (string, error) {
	var prev string
	if err := tx.Get(&prev, `SELECT status FROM supplier_nodes WHERE id=? FOR UPDATE`, id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE supplier_nodes SET status='online', last_heartbeat_at=NOW(), last_probe_ok_at=NOW(),
		probe_fail_count=0, available_cards=?, gpu_util_pct=?, vram_util_pct=? WHERE id=? AND mgmt_mode='ssh'`,
		available, gpuUtil, vramUtil, id); err != nil {
		return "", err
	}
	_, err := tx.Exec(`INSERT INTO node_heartbeats (node_id, available_cards, gpu_util_pct, vram_util_pct) VALUES (?,?,?,?)`,
		id, available, gpuUtil, vramUtil)
	return prev, err
}

// recordLightFail 轻量探活失败: 计数 +1; 距上次成功超过 ttl 判 offline。返回是否本次刚转为 offline。
func (r *Repository) recordLightFail(id int64, ttl time.Duration) (bool, error) {
	if _, err := r.db.Exec(`UPDATE supplier_nodes SET probe_fail_count=probe_fail_count+1 WHERE id=? AND mgmt_mode='ssh'`, id); err != nil {
		return false, err
	}
	return affected(r.db.Exec(`UPDATE supplier_nodes SET status='offline'
		WHERE id=? AND mgmt_mode='ssh' AND status<>'offline'
		  AND (last_probe_ok_at IS NULL OR last_probe_ok_at < NOW() - INTERVAL ? SECOND)`, id, int(ttl.Seconds())))
}

// ===== gpu_slots =====

func (r *Repository) listSlots(q sqlx.Queryer, nodeID int64) ([]Slot, error) {
	var list []Slot
	err := sqlx.Select(q, &list, `SELECT id, node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group, state, lease_id,
		state_changed_at FROM gpu_slots WHERE node_id=? ORDER BY gpu_index`, nodeID)
	return list, err
}

// uuidOwners 查找已被其他节点登记的卡 UUID(防一机多挂)。
func (r *Repository) uuidOwners(q sqlx.Queryer, nodeID int64, uuids []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(uuids) == 0 {
		return out, nil
	}
	query, args, err := sqlx.In(`SELECT gpu_uuid, node_id FROM gpu_slots WHERE gpu_uuid IN (?) AND node_id<>?`, uuids, nodeID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Queryx(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		var id int64
		if err := rows.Scan(&u, &id); err != nil {
			return nil, err
		}
		out[u] = id
	}
	return out, rows.Err()
}

// syncSlots 完整探活通过后按实测清单同步卡槽(同一事务):
//   - 新卡插入, 健康→free、故障→faulty;
//   - 已有卡更新硬件信息; free/faulty 按本次健康结论切换, reserved/leased/draining/withheld 不动
//     (在租卡故障的资源异常处理属 S2, 见 docs/25 §25.4.4);
//   - 清单中消失的卡 → faulty。
func (r *Repository) syncSlots(tx *sqlx.Tx, nodeID int64, inv *Inventory) error {
	existing, err := r.listSlots(tx, nodeID)
	if err != nil {
		return err
	}
	seen := map[int]bool{}
	for _, g := range inv.GPUs {
		seen[g.Index] = true
		state := SlotFree
		if _, bad := inv.Faulty[g.Index]; bad {
			state = SlotFaulty
		}
		// UUID 变化(换卡)的同 index 卡槽视为新卡: 先删旧行再插入, 避免旧 UUID 占用唯一键。
		if _, err := tx.Exec(`DELETE FROM gpu_slots WHERE node_id=? AND gpu_index=? AND gpu_uuid<>? AND state IN ('free','faulty','withheld')`,
			nodeID, g.Index, g.UUID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO gpu_slots (node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group, state)
			VALUES (?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE
			  pci_bus_id=VALUES(pci_bus_id), vram_mb=VALUES(vram_mb), topo_group=VALUES(topo_group),
			  state_changed_at = IF(state IN ('free','faulty') AND state<>VALUES(state), NOW(), state_changed_at),
			  state = IF(state IN ('free','faulty'), VALUES(state), state)`,
			nodeID, g.Index, g.UUID, g.PCIBusID, g.MemoryMiB, inv.TopoGroups[g.Index], state)
		if err != nil {
			return err
		}
	}
	for _, s := range existing {
		if !seen[s.GPUIndex] && s.State != SlotFaulty {
			if _, err := tx.Exec(`UPDATE gpu_slots SET state='faulty', state_changed_at=NOW() WHERE id=? AND state=?`, s.ID, s.State); err != nil {
				return err
			}
		}
	}
	return nil
}

// markSlotsFaulty 周期探活发现故障/掉卡: 空闲卡槽 → faulty, 返回实际变更数。
// 在租/预留/排空卡的"资源异常"处理(leases.resource_alert + 停机计时)属 S2(docs/25 §25.4.4), 此处不改其状态。
func (r *Repository) markSlotsFaulty(tx *sqlx.Tx, nodeID int64, indexes []int) (int64, error) {
	if len(indexes) == 0 {
		return 0, nil
	}
	q, args, err := sqlx.In(`UPDATE gpu_slots SET state='faulty', state_changed_at=NOW()
		WHERE node_id=? AND gpu_index IN (?) AND state='free'`, nodeID, indexes)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// setSlotState 供给方停用/启用单卡: 仅 free↔withheld。
func (r *Repository) setSlotState(tx *sqlx.Tx, nodeID int64, idx int, from, to string) (bool, error) {
	return affected(tx.Exec(`UPDATE gpu_slots SET state=?, state_changed_at=NOW() WHERE node_id=? AND gpu_index=? AND state=?`,
		to, nodeID, idx, from))
}

// deleteIdleSlots 删除/停用节点时释放卡 UUID 占用(仅空闲/停用/故障卡槽)。
func (r *Repository) deleteIdleSlots(tx *sqlx.Tx, nodeID int64) error {
	_, err := tx.Exec(`DELETE FROM gpu_slots WHERE node_id=? AND (state IN ('free','withheld') OR (state='faulty' AND lease_id IS NULL))`, nodeID)
	return err
}

// freeSlotStats 托管商品派生库存(docs/26 §26.5.4): verified+可调度+在线的 ssh 节点上的空闲卡合计,
// 以及单机最多可一次租用的卡数 max_free_per_node。后者与 allocCandidates 的候选规则同口径:
// 分卡容器且节点支持磁盘配额 → 该节点空闲数; 不支持配额的节点或整机交付 → 仅整机全空闲时为总卡数, 否则 0。
func (r *Repository) freeSlotStats(q sqlx.Queryer, productID int64) (free, maxNode int, err error) {
	var rows []struct {
		Free         int    `db:"free_cards"`
		TotalCards   int    `db:"total_cards"`
		Quota        bool   `db:"quota"`
		DeliveryMode string `db:"delivery_mode"`
	}
	err = sqlx.Select(q, &rows, `SELECT COUNT(s.id) AS free_cards, n.total_cards, COALESCE(n.disk_quota_supported,0)=1 AS quota,
		COALESCE(p.delivery_mode,'') AS delivery_mode
		FROM gpu_slots s JOIN supplier_nodes n ON n.id=s.node_id JOIN products p ON p.id=n.product_id
		WHERE n.product_id=? AND n.mgmt_mode='ssh' AND n.verify_status='verified' AND n.schedulable=1
		  AND n.status='online' AND s.state='free'
		GROUP BY n.id, n.total_cards, n.disk_quota_supported, p.delivery_mode`, productID)
	if err != nil {
		return 0, 0, err
	}
	for _, x := range rows {
		free += x.Free
		if m := rentableOnNode(x.Free, x.TotalCards, x.Quota, x.DeliveryMode == "bare_metal"); m > maxNode {
			maxNode = m
		}
	}
	return free, maxNode, nil
}

// rentableOnNode 单节点一次最多可租卡数(与 allocCandidates 同口径)。
func rentableOnNode(free, total int, quota, wholeMachine bool) int {
	if !wholeMachine && quota {
		return free
	}
	if free == total {
		return total
	}
	return 0
}

func (r *Repository) freeSlotUUIDs(q sqlx.Queryer, nodeID int64) (map[int]string, error) {
	var list []Slot
	if err := sqlx.Select(q, &list, `SELECT id, node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group, state, lease_id,
		state_changed_at FROM gpu_slots WHERE node_id=? AND state='free'`, nodeID); err != nil {
		return nil, err
	}
	out := map[int]string{}
	for _, s := range list {
		out[s.GPUIndex] = s.GPUUUID
	}
	return out, nil
}

// ===== gpu_catalog =====

func (r *Repository) catalogEntry(model string) (*CatalogEntry, error) {
	var c CatalogEntry
	err := r.db.Get(&c, `SELECT model_name, vendor, vram_gb, probe_baseline_tflops FROM gpu_catalog WHERE model_name=?`,
		strings.TrimSpace(model))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// ===== audit =====

// audit 写审计。operatorID=0 表示系统(探活发现指纹变化等), 落库为 NULL。
func (r *Repository) audit(q sqlx.Execer, operatorID int64, action, targetType string, targetID int64, before, after any, ip string) error {
	b, a := auditText(before), auditText(after)
	var op any
	if operatorID > 0 {
		op = operatorID
	}
	_, err := q.Exec(`INSERT INTO audit_logs (operator_id, action, target_type, target_id, before_value, after_value, ip)
		VALUES (?,?,?,?,?,?,?)`, op, action, targetType, targetID, b, a, nullStr(ip))
	return err
}

func auditText(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return nullStr(t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return nil
		}
		return string(raw)
	}
}
