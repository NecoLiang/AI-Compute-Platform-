package modeldeploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

// SetDeployRuntime 注入一键部署运行期(fleet 节点任务队列/SSH 通道)、站内通知与 Redis(日志限流)。
// fleet.enabled=false 时 main.go 不调用, 提交部署返回 50000 并说明缺什么。
func (s *Service) SetDeployRuntime(rt Runtime, n Notifier, rdb *redis.Client) {
	s.rt, s.notifier, s.rdb = rt, n, rdb
}

// runtimeReady 提交/重启前确认运行期依赖齐全; 缺哪项就明说哪项。
func (s *Service) runtimeReady() error {
	switch {
	case s.leases == nil:
		return newErr(errcode.InternalError, "托管租约能力尚未上线(S2 未接入), 暂无法部署")
	case s.rt == nil:
		return newErr(errcode.InternalError, "一键部署运行期未接入(fleet 未启用), 暂无法部署")
	}
	st := s.rt.Settings()
	switch {
	case st.CacheDir == "":
		return newErr(errcode.InternalError, "平台未配置模型缓存目录 fleet.model_cache_dir, 暂无法部署")
	case st.PullImage == "":
		return newErr(errcode.InternalError, "平台未配置模型拉取镜像 fleet.model_pull_image, 暂无法部署")
	case st.PortStart <= 0 || st.PortEnd < st.PortStart:
		return newErr(errcode.InternalError, "平台未配置引擎端口区间 fleet.engine_port_range, 暂无法部署")
	}
	return nil
}

// SubmitRequest POST /orders/:id/deployments。
type SubmitRequest struct {
	BuyerID       int64
	OrderNo       string
	ModelID       int64
	TP, DP        int
	MaxModelLen   int // 0 = 取方案上限
	GPUMemUtilPct int
	CanaryEnabled bool
	LicenseAck    bool
	UntestedAck   bool
}

// orderRef 订单号 → (id, buyer)。他人订单与不存在同样返回 40400。
func (s *Service) orderRef(buyerID int64, orderNo string) (int64, error) {
	if !orderNoPattern.MatchString(orderNo) {
		return 0, newErr(errcode.ParamInvalid, "订单号格式无效")
	}
	var o struct {
		ID      int64 `db:"id"`
		BuyerID int64 `db:"buyer_id"`
	}
	err := s.repo.db.Get(&o, `SELECT id, buyer_id FROM orders WHERE order_no=?`, orderNo)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.BuyerID != buyerID) {
		return 0, newErr(errcode.NotFound, "订单不存在")
	}
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

func liveDeploymentOfOrder(q sqlx.Queryer, orderID int64) (*Deployment, error) {
	ph, args := inList(liveStates)
	var d Deployment
	err := sqlx.Get(q, &d, "SELECT "+deploymentColumns+" FROM model_deployments WHERE order_id=? AND state IN ("+ph+") LIMIT 1",
		append([]any{orderID}, args...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// Submit 提交部署: 服务端重新做实时可行性判定(不信任前端方案), 同一租约至多一个未终止部署
// (uk_lease_active 唯一键兜底并发), 状态写入与 deploy_model 任务入队同一事务。
func (s *Service) Submit(ctx context.Context, req SubmitRequest) (*DeploymentView, error) {
	if req.ModelID <= 0 {
		return nil, newErr(errcode.ParamInvalid, "model_id 必填")
	}
	if req.TP < 1 || req.DP < 1 {
		return nil, newErr(errcode.ParamInvalid, "tp/dp 须为正整数")
	}
	if req.MaxModelLen < 0 {
		return nil, newErr(errcode.ParamInvalid, "max_model_len 不得为负")
	}
	if err := s.runtimeReady(); err != nil {
		return nil, err
	}
	orderID, err := s.orderRef(req.BuyerID, req.OrderNo)
	if err != nil {
		return nil, err
	}
	if cur, err := liveDeploymentOfOrder(s.repo.db, orderID); err != nil {
		return nil, err
	} else if cur != nil {
		return nil, &Error{Code: errcode.Conflict, Msg: "该租约已有未终止的部署 " + cur.DeployNo + ", 请先停止",
			Data: map[string]any{"deploy_no": cur.DeployNo, "state": cur.State}}
	}
	ev, err := s.evaluateOrder(ctx, PlanRequest{BuyerID: req.BuyerID, OrderNo: req.OrderNo, ModelID: req.ModelID, GPUMemUtilPct: req.GPUMemUtilPct})
	if err != nil {
		return nil, err
	}
	res := ev.res
	if !res.Feasible {
		return nil, &Error{Code: errcode.ParamInvalid, Msg: "当前租约不满足部署条件", Data: map[string]any{"checks": res.Checks}}
	}
	if missing := res.MissingAcks(req.LicenseAck, req.UntestedAck); len(missing) > 0 {
		return nil, &Error{Code: errcode.ParamInvalid, Msg: "须先确认许可证/未验证卡型提示", Data: map[string]any{"checks": missing}}
	}
	var plan *Plan
	for i := range res.Plans {
		if res.Plans[i].TP == req.TP && res.Plans[i].DP == req.DP {
			plan = &res.Plans[i]
		}
	}
	if plan == nil {
		return nil, &Error{Code: errcode.ParamInvalid, Msg: fmt.Sprintf("tp=%d dp=%d 不在可行方案内", req.TP, req.DP),
			Data: map[string]any{"plans": res.Plans}}
	}
	mml := req.MaxModelLen
	if mml == 0 {
		mml = plan.MaxModelLen
	}
	if mml > plan.MaxModelLen || mml < ev.entry.MinContextTokens {
		return nil, newErr(errcode.ParamInvalid, "max_model_len 须在 %d–%d 之间", ev.entry.MinContextTokens, plan.MaxModelLen)
	}
	need := req.TP * req.DP
	uuids := make([]string, 0, need)
	for _, c := range ev.snap.Cards {
		if len(uuids) < need && !c.Faulty {
			uuids = append(uuids, c.UUID)
		}
	}
	if len(uuids) < need {
		return nil, newErr(errcode.Conflict, "租约可用卡数不足 %d 张", need)
	}
	uuidsRaw, _ := json.Marshal(uuids)
	feas, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	now := s.now()
	d := &Deployment{LeaseID: ev.snap.LeaseID, OrderID: ev.snap.OrderID, BuyerID: req.BuyerID, NodeID: ev.snap.NodeID,
		ModelID: ev.entry.ID, GPUUUIDsRaw: uuidsRaw, TP: req.TP, DP: req.DP, MaxModelLen: mml,
		GPUMemUtil: float64(req.GPUMemUtilPct) / 100, CanaryEnabled: req.CanaryEnabled, Feasibility: feas}
	if req.LicenseAck {
		d.LicenseAckAt = &now
	}
	if req.UntestedAck {
		d.UntestedAckAt = &now
	}
	if d.OrderID != orderID {
		return nil, fmt.Errorf("lease source order id %d != order %s id %d", d.OrderID, req.OrderNo, orderID)
	}

	for attempt := 0; ; attempt++ {
		no, err := newDeployNo()
		if err != nil {
			return nil, err
		}
		d.DeployNo = no
		id, err := s.insertAndEnqueue(ctx, d)
		if isDupKey(err, "deploy_no") && attempt < 3 {
			continue
		}
		if isDupKey(err, "uk_lease_active") {
			return nil, newErr(errcode.Conflict, "该租约已有未终止的部署, 请先停止")
		}
		if err != nil {
			return nil, err
		}
		return s.viewByID(id)
	}
}

func (s *Service) insertAndEnqueue(ctx context.Context, d *Deployment) (int64, error) {
	tx, err := s.repo.beginTx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := insertDeploymentTx(tx, d)
	if err != nil {
		return 0, err
	}
	if err := s.enqueueDeploy(ctx, tx, d.NodeID, d.LeaseID, id); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Service) enqueueDeploy(ctx context.Context, tx *sqlx.Tx, nodeID, leaseID, depID int64) error {
	return s.rt.Enqueue(ctx, tx, TaskSpec{NodeID: nodeID, LeaseID: &leaseID, DeploymentID: &depID,
		Kind: TaskDeployModel, Priority: 5, MaxAttempts: 3, Payload: map[string]any{"deployment_id": depID}})
}

func (s *Service) enqueueStop(ctx context.Context, tx *sqlx.Tx, d *Deployment, final string) error {
	leaseID, depID := d.LeaseID, d.ID
	return s.rt.Enqueue(ctx, tx, TaskSpec{NodeID: d.NodeID, LeaseID: &leaseID, DeploymentID: &depID,
		Kind: TaskStopModel, Priority: 2, MaxAttempts: 5, Payload: map[string]any{"deployment_id": depID, "final": final}})
}

// DeploymentView 买家/运营看到的部署。
type DeploymentView struct {
	DeployNo         string            `json:"deploy_no"`
	OrderNo          string            `json:"order_no"`
	Model            ModelRef          `json:"model"`
	State            string            `json:"state"`
	Health           string            `json:"health"`
	HealthReason     *string           `json:"health_reason"`
	HealthReasonText *string           `json:"health_reason_text"`
	TP               int               `json:"tp"`
	DP               int               `json:"dp"`
	MaxModelLen      int               `json:"max_model_len"`
	GPUMemUtil       float64           `json:"gpu_mem_util"`
	GPUUUIDs         []string          `json:"gpu_uuids"`
	CanaryEnabled    bool              `json:"canary_enabled"`
	EngineCPUs       *int              `json:"engine_cpus"`
	EngineMemoryGB   *int              `json:"engine_memory_gb"`
	Access           *AccessInfo       `json:"access"`
	Cache            *CacheProgress    `json:"cache"`
	RestartCount     int               `json:"restart_count"`
	LastRestartAt    *time.Time        `json:"last_restart_at"`
	NextRestartAt    *time.Time        `json:"next_restart_at"`
	FailReason       *string           `json:"fail_reason"`
	StopReason       *string           `json:"stop_reason"`
	StartedAt        *time.Time        `json:"started_at"`
	StoppedAt        *time.Time        `json:"stopped_at"`
	Events           []DeploymentEvent `json:"events,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	// 运营列表附带。
	BuyerID *int64 `json:"buyer_id,omitempty"`
	NodeID  *int64 `json:"node_id,omitempty"`
}

type ModelRef struct {
	ID          int64  `json:"id"`
	ModelKey    string `json:"model_key"`
	DisplayName string `json:"display_name"`
}

// AccessInfo 引擎只在租约网络内可达(docs/27 §27.4.2): 租约容器内用 base_url; 本地经 SSH 端口转发。
type AccessInfo struct {
	BaseURL        string `json:"base_url"`
	ServedModel    string `json:"served_model"`
	PortForwardCmd string `json:"port_forward_cmd,omitempty"`
}

type CacheProgress struct {
	State        string `json:"state"`
	BytesDone    int64  `json:"bytes_done"`
	WeightsBytes int64  `json:"weights_bytes"`
}
