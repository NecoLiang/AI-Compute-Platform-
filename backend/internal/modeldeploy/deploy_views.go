package modeldeploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

// 买家读接口 + 运营列表。

func (s *Service) viewByID(id int64) (*DeploymentView, error) {
	d, err := getDeployment(s.repo.db, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("deployment %d vanished", id)
	}
	return s.toView(d, true, false)
}

type viewRefs struct {
	OrderNo string `db:"order_no"`
	LeaseNo string `db:"lease_no"`
}

func (s *Service) toView(d *Deployment, withEvents, admin bool) (*DeploymentView, error) {
	var refs viewRefs
	if err := s.repo.db.Get(&refs, `SELECT o.order_no, COALESCE(l.lease_no,'') AS lease_no FROM orders o
		LEFT JOIN leases l ON l.id=? WHERE o.id=?`, d.LeaseID, d.OrderID); err != nil {
		return nil, err
	}
	e, err := s.repo.GetEntry(d.ModelID)
	if err != nil {
		return nil, err
	}
	v := &DeploymentView{
		DeployNo: d.DeployNo, OrderNo: refs.OrderNo, State: d.State, Health: d.Health, HealthReason: d.HealthReason,
		TP: d.TP, DP: d.DP, MaxModelLen: d.MaxModelLen, GPUMemUtil: d.GPUMemUtil, GPUUUIDs: d.GPUUUIDs(),
		CanaryEnabled: d.CanaryEnabled, EngineCPUs: d.EngineCPUs, EngineMemoryGB: d.EngineMemoryGB,
		RestartCount: d.RestartCount, LastRestartAt: d.LastRestartAt, NextRestartAt: d.NextRestartAt,
		FailReason: d.FailReason, StopReason: d.StopReason, StartedAt: d.StartedAt, StoppedAt: d.StoppedAt,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
	if v.GPUUUIDs == nil {
		v.GPUUUIDs = []string{}
	}
	if d.HealthReason != nil {
		v.HealthReasonText = strp(ReasonText(*d.HealthReason))
	}
	if e != nil {
		v.Model = ModelRef{ID: e.ID, ModelKey: e.ModelKey, DisplayName: e.DisplayName}
	} else {
		v.Model = ModelRef{ID: d.ModelID}
	}
	if e != nil && (d.State == DepRunning || d.State == DepRestarting || d.State == DepStarting) {
		v.Access = &AccessInfo{BaseURL: "http://" + engineNetworkAlias + ":8000/v1", ServedModel: e.ModelKey}
		if s.rt != nil {
			if st := s.rt.Settings(); st.GatewayHost != "" && st.GatewayPort > 0 && refs.LeaseNo != "" {
				v.Access.PortForwardCmd = fmt.Sprintf("ssh -p %d -N -L 8000:%s:8000 %s@%s",
					st.GatewayPort, engineNetworkAlias, refs.LeaseNo, st.GatewayHost)
			}
		}
	}
	if e != nil && d.State == DepPulling {
		c, err := getCache(s.repo.db, d.NodeID, d.ModelID)
		if err != nil {
			return nil, err
		}
		if c != nil {
			v.Cache = &CacheProgress{State: c.State, BytesDone: c.BytesDone, WeightsBytes: e.WeightsBytes}
		}
	}
	if withEvents {
		if v.Events, err = recentEvents(s.repo.db, d.ID, 20); err != nil {
			return nil, err
		}
	}
	if admin {
		v.BuyerID, v.NodeID = &d.BuyerID, &d.NodeID
	}
	return v, nil
}

// ListDeployments GET /orders/:id/deployments(新→旧, 不含事件)。
func (s *Service) ListDeployments(buyerID int64, orderNo string) ([]DeploymentView, error) {
	orderID, err := s.orderRef(buyerID, orderNo)
	if err != nil {
		return nil, err
	}
	list, err := listDeploymentsByOrder(s.repo.db, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]DeploymentView, 0, len(list))
	for i := range list {
		v, err := s.toView(&list[i], false, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

func (s *Service) buyerDeployment(buyerID int64, orderNo, deployNo string) (*Deployment, error) {
	if !ValidDeployNo(deployNo) {
		return nil, newErr(errcode.NotFound, "部署不存在")
	}
	orderID, err := s.orderRef(buyerID, orderNo)
	if err != nil {
		return nil, err
	}
	d, err := getDeploymentByNo(s.repo.db, orderID, deployNo)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, newErr(errcode.NotFound, "部署不存在")
	}
	return d, nil
}

// GetDeployment GET /orders/:id/deployments/:deploy_no
func (s *Service) GetDeployment(buyerID int64, orderNo, deployNo string) (*DeploymentView, error) {
	d, err := s.buyerDeployment(buyerID, orderNo, deployNo)
	if err != nil {
		return nil, err
	}
	return s.toView(d, true, false)
}

// HealthRanges range → (窗口, 降采样桶秒数)。1h 原始点(60s 一个); 24h 5 分钟桶; 7d 1 小时桶。
var HealthRanges = map[string][2]int{"1h": {3600, 0}, "24h": {86400, 300}, "7d": {7 * 86400, 3600}}

type HealthSeriesView struct {
	DeployNo         string      `json:"deploy_no"`
	Range            string      `json:"range"`
	BucketSeconds    int         `json:"bucket_seconds"`
	Health           string      `json:"health"`
	HealthReason     *string     `json:"health_reason"`
	HealthReasonText *string     `json:"health_reason_text"`
	BaselineCanaryMS *int        `json:"baseline_canary_ms"`
	Points           []SampleRow `json:"points"`
}

// HealthSeries GET /orders/:id/deployments/:deploy_no/health?range=
func (s *Service) HealthSeries(buyerID int64, orderNo, deployNo, rng string) (*HealthSeriesView, error) {
	if rng == "" {
		rng = "1h"
	}
	r, ok := HealthRanges[rng]
	if !ok {
		return nil, newErr(errcode.ParamInvalid, "range 须为 1h/24h/7d")
	}
	d, err := s.buyerDeployment(buyerID, orderNo, deployNo)
	if err != nil {
		return nil, err
	}
	pts, err := healthSeries(s.repo.db, d.ID, s.now().Add(-time.Duration(r[0])*time.Second), r[1])
	if err != nil {
		return nil, err
	}
	v := &HealthSeriesView{DeployNo: d.DeployNo, Range: rng, BucketSeconds: r[1], Health: d.Health,
		HealthReason: d.HealthReason, BaselineCanaryMS: d.BaselineCanaryMS, Points: pts}
	if r[1] == 0 {
		v.BucketSeconds = 60
	}
	if d.HealthReason != nil {
		v.HealthReasonText = strp(ReasonText(*d.HealthReason))
	}
	return v, nil
}

// LogsPerMinute 日志接口每买家每部署每分钟 6 次(docs/27 §27.8)。
const LogsPerMinute = 6

var fixedWindowIncr = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return n`)

type LogsView struct {
	DeployNo string   `json:"deploy_no"`
	Source   string   `json:"source"` // live = 实时 docker logs; stored = 失败时留存的末尾日志
	Lines    []string `json:"lines"`
}

// Logs GET /orders/:id/deployments/:deploy_no/logs?tail= · 限流 + 审计。
func (s *Service) Logs(ctx context.Context, buyerID int64, orderNo, deployNo string, tail int, ip string) (*LogsView, error) {
	if tail < 1 || tail > 1000 {
		return nil, newErr(errcode.ParamInvalid, "tail 须在 1–1000 之间")
	}
	d, err := s.buyerDeployment(buyerID, orderNo, deployNo)
	if err != nil {
		return nil, err
	}
	if s.rdb == nil {
		return nil, newErr(errcode.InternalError, "日志接口限流依赖 Redis, 当前未接入")
	}
	key := fmt.Sprintf("modeldeploy:logs:rl:%d:%d", buyerID, d.ID)
	n, err := fixedWindowIncr.Run(ctx, s.rdb, []string{key}, int64(time.Minute/time.Millisecond)).Int64()
	if err != nil {
		return nil, err
	}
	if n > LogsPerMinute {
		return nil, newErr(errcode.TooManyRequests, "查看日志过于频繁, 每分钟最多 %d 次", LogsPerMinute)
	}
	if _, err := s.repo.db.Exec(`INSERT INTO audit_logs (operator_id, action, target_type, target_id, after_value, ip)
		VALUES (?, 'model_deploy.view_logs', 'model_deployment', ?, ?, ?)`,
		buyerID, d.ID, fmt.Sprintf(`{"deploy_no":%q,"tail":%d}`, d.DeployNo, tail), ip); err != nil {
		return nil, err
	}
	v := &LogsView{DeployNo: d.DeployNo, Source: "stored", Lines: []string{}}
	if isOneOf(d.State, []string{DepStarting, DepRunning, DepRestarting}) && s.rt != nil {
		if lines, ok, err := s.liveLogs(ctx, d, tail); err == nil && ok {
			v.Source, v.Lines = "live", lines
			return v, nil
		}
		// 节点不可达/容器已不在: 退回留存日志, 不报错(与健康"不可达=未知"同口径)。
	}
	if d.LogTail != nil && *d.LogTail != "" {
		v.Lines = strings.Split(tailLines(*d.LogTail, tail), "\n")
	}
	return v, nil
}

func (s *Service) liveLogs(ctx context.Context, d *Deployment, tail int) ([]string, bool, error) {
	cmd, err := engineLogsCommand(d.DeployNo, tail)
	if err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sh, err := s.rt.Dial(ctx, d.NodeID)
	if err != nil {
		return nil, false, err
	}
	defer sh.Close()
	r, err := sh.Run(ctx, cmd)
	if err != nil || r.ExitCode != 0 {
		return nil, false, fmt.Errorf("docker logs: %v exit=%d", err, r.ExitCode)
	}
	out := strings.TrimRight(r.Stdout, "\n")
	if out == "@@absent" {
		return nil, false, nil
	}
	if out == "" {
		return []string{}, true, nil
	}
	return strings.Split(tailLines(out, tail), "\n"), true, nil
}

// AdminDeployments GET /admin/deployments?state=&health=&page=&page_size=
func (s *Service) AdminDeployments(state, health string, page, pageSize int) ([]DeploymentView, int, error) {
	if state != "" && !validDepStates[state] {
		return nil, 0, newErr(errcode.ParamInvalid, "state 取值非法")
	}
	if health != "" && !validHealth[health] {
		return nil, 0, newErr(errcode.ParamInvalid, "health 取值非法")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	where, args := "1=1", []any{}
	if state != "" {
		where += " AND state=?"
		args = append(args, state)
	}
	if health != "" {
		where += " AND health=?"
		args = append(args, health)
	}
	var total int
	if err := sqlx.Get(s.repo.db, &total, "SELECT COUNT(*) FROM model_deployments WHERE "+where, args...); err != nil {
		return nil, 0, err
	}
	list := []Deployment{}
	if err := sqlx.Select(s.repo.db, &list, "SELECT "+deploymentColumns+" FROM model_deployments WHERE "+where+
		" ORDER BY id DESC LIMIT ? OFFSET ?", append(args, pageSize, (page-1)*pageSize)...); err != nil {
		return nil, 0, err
	}
	out := make([]DeploymentView, 0, len(list))
	for i := range list {
		v, err := s.toView(&list[i], false, true)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *v)
	}
	return out, total, nil
}
