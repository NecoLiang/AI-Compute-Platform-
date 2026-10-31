package modeldeploy

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// model_deployments / node_model_cache / deployment_events / deployment_health_samples 的读写。
// 所有状态迁移都是守卫式 UPDATE(WHERE state IN 允许的来源), 以 RowsAffected 判定是否生效。

func getDeployment(q sqlx.Queryer, id int64) (*Deployment, error) {
	var d Deployment
	err := sqlx.Get(q, &d, "SELECT "+deploymentColumns+" FROM model_deployments WHERE id=?", id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func lockDeploymentTx(tx *sqlx.Tx, id int64) (*Deployment, error) {
	var d Deployment
	err := tx.Get(&d, "SELECT "+deploymentColumns+" FROM model_deployments WHERE id=? FOR UPDATE", id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// getDeploymentByNo 按订单限定(订单归属已由调用方校验), 他人部署号与不存在同样返回 nil。
func getDeploymentByNo(q sqlx.Queryer, orderID int64, deployNo string) (*Deployment, error) {
	var d Deployment
	err := sqlx.Get(q, &d, "SELECT "+deploymentColumns+" FROM model_deployments WHERE order_id=? AND deploy_no=?", orderID, deployNo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func listDeploymentsByOrder(q sqlx.Queryer, orderID int64) ([]Deployment, error) {
	list := []Deployment{}
	err := sqlx.Select(q, &list, "SELECT "+deploymentColumns+" FROM model_deployments WHERE order_id=? ORDER BY id DESC", orderID)
	return list, err
}

func listDeploymentsInStates(q sqlx.Queryer, states []string) ([]Deployment, error) {
	ph, args := inList(states)
	list := []Deployment{}
	err := sqlx.Select(q, &list, "SELECT "+deploymentColumns+" FROM model_deployments WHERE state IN ("+ph+") ORDER BY id", args...)
	return list, err
}

func insertDeploymentTx(tx *sqlx.Tx, d *Deployment) (int64, error) {
	res, err := tx.Exec(`INSERT INTO model_deployments (deploy_no, lease_id, order_id, buyer_id, node_id, model_id, gpu_uuids,
		tp, dp, max_model_len, gpu_mem_util, lease_active_claim, state, state_changed_at, health, canary_enabled,
		feasibility, license_ack_at, untested_ack_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'pending',NOW(),'unknown',?,?,?,?)`,
		d.DeployNo, d.LeaseID, d.OrderID, d.BuyerID, d.NodeID, d.ModelID, string(d.GPUUUIDsRaw),
		d.TP, d.DP, d.MaxModelLen, fmt.Sprintf("%.2f", d.GPUMemUtil), d.LeaseID, d.CanaryEnabled,
		string(d.Feasibility), d.LicenseAckAt, d.UntestedAckAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// isDupKey MySQL 1062, 按唯一键名区分(uk_lease_active / uk_node_engine_port / deploy_no / uk_node_model)。
func isDupKey(err error, key string) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry") && strings.Contains(err.Error(), key)
}

// setState 守卫式状态迁移, 附带额外 SET 片段(如 "fail_reason=?")。返回是否生效。
func setState(q sqlx.Execer, id int64, to string, from []string, extra string, extraArgs ...any) (bool, error) {
	ph, fromArgs := inList(from)
	set := "state=?, state_changed_at=NOW()"
	if extra != "" {
		set += ", " + extra
	}
	args := append([]any{to}, extraArgs...)
	args = append(args, id)
	args = append(args, fromArgs...)
	res, err := q.Exec("UPDATE model_deployments SET "+set+" WHERE id=? AND state IN ("+ph+")", args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func insertEvent(q sqlx.Execer, depID int64, kind, reason, detail string) error {
	_, err := q.Exec(`INSERT INTO deployment_events (deployment_id, kind, reason, detail) VALUES (?,?,?,?)`,
		depID, kind, nullIfEmpty(reason), nullIfEmpty(truncateStr(detail, 512)))
	return err
}

func countEventsSince(q sqlx.Queryer, depID int64, kind string, since time.Time) (int, error) {
	var n int
	err := sqlx.Get(q, &n, `SELECT COUNT(*) FROM deployment_events WHERE deployment_id=? AND kind=? AND created_at>=?`, depID, kind, since)
	return n, err
}

// DeploymentEvent 部署事件(详情页重启记录)。
type DeploymentEvent struct {
	Kind      string    `db:"kind" json:"kind"`
	Reason    *string   `db:"reason" json:"reason"`
	Detail    *string   `db:"detail" json:"detail"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

func recentEvents(q sqlx.Queryer, depID int64, limit int) ([]DeploymentEvent, error) {
	list := []DeploymentEvent{}
	err := sqlx.Select(q, &list, `SELECT kind, reason, detail, created_at FROM deployment_events
		WHERE deployment_id=? AND kind NOT IN ('degraded_notice','unhealthy_notice') ORDER BY id DESC LIMIT ?`, depID, limit)
	return list, err
}

// taskInFlight 部署是否有未结束的指定类型节点任务(node_tasks 由 fleet 执行)。
func taskInFlight(q sqlx.Queryer, depID int64, kind string) (bool, error) {
	var n int
	err := sqlx.Get(q, &n, `SELECT COUNT(*) FROM node_tasks WHERE deployment_id=? AND kind=? AND state IN ('pending','running')`, depID, kind)
	return n > 0, err
}

// CacheRow node_model_cache 一行。
type CacheRow struct {
	ID            int64      `db:"id"`
	NodeID        int64      `db:"node_id"`
	ModelID       int64      `db:"model_id"`
	State         string     `db:"state"`
	BytesDone     int64      `db:"bytes_done"`
	VerifiedAt    *time.Time `db:"verified_at"`
	LastUsedAt    *time.Time `db:"last_used_at"`
	LastError     *string    `db:"last_error"`
	PullContainer *string    `db:"pull_container"`
	PullStartedAt *time.Time `db:"pull_started_at"`
}

const cacheColumns = `id, node_id, model_id, state, bytes_done, verified_at, last_used_at, last_error, pull_container, pull_started_at`

const (
	CacheDownloading = "downloading"
	CacheReady       = "ready"
	CacheFailed      = "failed"
	CacheEvicting    = "evicting"
)

func getCache(q sqlx.Queryer, nodeID, modelID int64) (*CacheRow, error) {
	var c CacheRow
	err := sqlx.Get(q, &c, "SELECT "+cacheColumns+" FROM node_model_cache WHERE node_id=? AND model_id=?", nodeID, modelID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func lockCacheTx(tx *sqlx.Tx, nodeID, modelID int64) (*CacheRow, error) {
	var c CacheRow
	err := tx.Get(&c, "SELECT "+cacheColumns+" FROM node_model_cache WHERE node_id=? AND model_id=? FOR UPDATE", nodeID, modelID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func lockCacheByIDTx(tx *sqlx.Tx, id int64) (*CacheRow, error) {
	var c CacheRow
	err := tx.Get(&c, "SELECT "+cacheColumns+" FROM node_model_cache WHERE id=? FOR UPDATE", id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// liveRefs 引用 (node, model) 缓存条目的未终止部署数(这些条目不可淘汰)。
func liveRefs(q sqlx.Queryer, nodeID, modelID int64) (int, error) {
	ph, args := inList(liveStates)
	var n int
	err := sqlx.Get(q, &n, `SELECT COUNT(*) FROM model_deployments WHERE node_id=? AND model_id=? AND state IN (`+ph+`)`,
		append([]any{nodeID, modelID}, args...)...)
	return n, err
}

// SampleRow deployment_health_samples 一行(读出)。
type SampleRow struct {
	SampledAt       time.Time `db:"sampled_at" json:"t"`
	ContainerUp     *float64  `db:"container_up" json:"container_up"`
	HealthOK        *float64  `db:"health_ok" json:"health_ok"`
	ModelsOK        *float64  `db:"models_ok" json:"models_ok"`
	CanaryOK        *float64  `db:"canary_ok" json:"canary_ok"`
	CanaryMS        *float64  `db:"canary_ms" json:"canary_ms"`
	RequestsRunning *float64  `db:"requests_running" json:"requests_running"`
	RequestsWaiting *float64  `db:"requests_waiting" json:"requests_waiting"`
	KVUsagePct      *float64  `db:"kv_usage_pct" json:"kv_usage_pct"`
	TTFTP50MS       *float64  `db:"ttft_p50_ms" json:"ttft_p50_ms"`
	TTFTP95MS       *float64  `db:"ttft_p95_ms" json:"ttft_p95_ms"`
	GenTPS          *float64  `db:"gen_tokens_per_s" json:"gen_tokens_per_s"`
	PromptTPS       *float64  `db:"prompt_tokens_per_s" json:"prompt_tokens_per_s"`
	Preemptions     *float64  `db:"preemptions" json:"preemptions"`
	GPUTempMaxC     *float64  `db:"gpu_temp_max_c" json:"gpu_temp_max_c"`
	Throttled       *float64  `db:"throttled" json:"throttled"`
	Health          string    `db:"health" json:"health"`
}

func insertSample(q sqlx.Execer, depID int64, at time.Time, s Sample) error {
	_, err := q.Exec(`INSERT INTO deployment_health_samples (deployment_id, sampled_at, container_up, health_ok, models_ok,
		canary_ok, canary_ms, requests_running, requests_waiting, kv_usage_pct, ttft_p50_ms, ttft_p95_ms,
		gen_tokens_per_s, prompt_tokens_per_s, preemptions, gpu_temp_max_c, throttled, health)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		depID, at, s.ContainerUp, s.HealthOK, s.ModelsOK, s.CanaryOK, s.CanaryMS, s.RequestsRunning, s.RequestsWaiting,
		s.KVUsagePct, s.TTFTP50MS, s.TTFTP95MS, s.GenTPS, s.PromptTPS, s.Preemptions, s.GPUTempMaxC, s.Throttled, s.Health)
	return err
}

// healthSeries 降采样: bucketSec=0 原始点; 否则按 FLOOR(UNIX_TIMESTAMP/bucket) 分桶,
// 数值取平均、布尔取比例(0–1), health 取桶内最差(ENUM 序 unknown<healthy<degraded<unhealthy)。
func healthSeries(q sqlx.Queryer, depID int64, since time.Time, bucketSec int) ([]SampleRow, error) {
	list := []SampleRow{}
	if bucketSec <= 0 {
		err := sqlx.Select(q, &list, `SELECT sampled_at, container_up, health_ok, models_ok, canary_ok, canary_ms,
			requests_running, requests_waiting, kv_usage_pct, ttft_p50_ms, ttft_p95_ms, gen_tokens_per_s,
			prompt_tokens_per_s, preemptions, gpu_temp_max_c, throttled, health
			FROM deployment_health_samples WHERE deployment_id=? AND sampled_at>=? ORDER BY sampled_at`, depID, since)
		return list, err
	}
	// bucketSec 只取自固定集合(300/3600), 直接写入 SQL; 分桶键放进派生表, 满足 ONLY_FULL_GROUP_BY。
	query := fmt.Sprintf(`SELECT FROM_UNIXTIME(b*%d) AS sampled_at,
		AVG(container_up) AS container_up, AVG(health_ok) AS health_ok, AVG(models_ok) AS models_ok,
		AVG(canary_ok) AS canary_ok, ROUND(AVG(canary_ms)) AS canary_ms,
		ROUND(AVG(requests_running),2) AS requests_running, ROUND(AVG(requests_waiting),2) AS requests_waiting,
		ROUND(AVG(kv_usage_pct),2) AS kv_usage_pct, ROUND(AVG(ttft_p50_ms)) AS ttft_p50_ms, MAX(ttft_p95_ms) AS ttft_p95_ms,
		ROUND(AVG(gen_tokens_per_s),2) AS gen_tokens_per_s, ROUND(AVG(prompt_tokens_per_s),2) AS prompt_tokens_per_s,
		SUM(preemptions) AS preemptions, MAX(gpu_temp_max_c) AS gpu_temp_max_c, MAX(throttled) AS throttled,
		ELT(MAX(h), 'unknown','healthy','degraded','unhealthy') AS health
		FROM (SELECT FLOOR(UNIX_TIMESTAMP(sampled_at)/%d) AS b, health+0 AS h, container_up, health_ok, models_ok,
			canary_ok, canary_ms, requests_running, requests_waiting, kv_usage_pct, ttft_p50_ms, ttft_p95_ms,
			gen_tokens_per_s, prompt_tokens_per_s, preemptions, gpu_temp_max_c, throttled
			FROM deployment_health_samples WHERE deployment_id=? AND sampled_at>=?) x
		GROUP BY b ORDER BY b`, bucketSec, bucketSec)
	err := sqlx.Select(q, &list, query, depID, since)
	return list, err
}

// SampleRetention 健康采样保留 7 天(docs/27 §27.5)。
const SampleRetention = 7 * 24 * time.Hour

// CleanupSamples 删除早于 now-7d 的采样(分批, 避免长事务)。返回删除行数。
func (s *Service) CleanupSamples(now time.Time) (int64, error) {
	cutoff := now.Add(-SampleRetention)
	var total int64
	for {
		res, err := s.repo.db.Exec(`DELETE FROM deployment_health_samples WHERE sampled_at < ? LIMIT 5000`, cutoff)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < 5000 {
			return total, nil
		}
	}
}

func saveMonitorState(q sqlx.Execer, id int64, st MonitorState, health, reason string, fromStates []string) (bool, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return false, err
	}
	ph, args := inList(fromStates)
	res, err := q.Exec(`UPDATE model_deployments SET monitor_state=?, health=?, health_reason=? WHERE id=? AND state IN (`+ph+`)`,
		append([]any{string(raw), health, nullIfEmpty(reason), id}, args...)...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *Deployment) monitorState() MonitorState {
	var st MonitorState
	if len(d.MonitorStateRaw) > 0 && string(d.MonitorStateRaw) != "null" {
		_ = json.Unmarshal(d.MonitorStateRaw, &st)
	}
	return st
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 按字节截断时避开 UTF-8 多字节中间。
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// tailLines 取最后 n 行(日志末尾 200 行)。
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
