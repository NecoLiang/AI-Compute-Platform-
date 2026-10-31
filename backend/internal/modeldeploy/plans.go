package modeldeploy

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"tokenfactory/pkg/errcode"
)

// orderNoPattern 与 compute 订单号格式一致(新购 ORD / 续租 REN)。
var orderNoPattern = regexp.MustCompile(`^(?:ORD|REN)[A-Za-z0-9-]{6,29}$`)

// SetLeaseSource 注入托管租约数据源(S2 internal/fleet)。未注入时 deploy-plans 返回 50000。
func (s *Service) SetLeaseSource(ls LeaseSource) { s.leases = ls }

// PublicModels GET /models: 目录中 active 条目, 附已验证卡型。
func (s *Service) PublicModels() ([]PublicModel, error) {
	entries, err := s.repo.ListEntries(StatusActive)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	passed, err := s.repo.PassedGPUModels(ids)
	if err != nil {
		return nil, err
	}
	out := make([]PublicModel, 0, len(entries))
	for _, e := range entries {
		tps, err := ParseValidTP(e.ValidTP)
		if err != nil {
			return nil, fmt.Errorf("目录条目 %d valid_tp 损坏: %w", e.ID, err)
		}
		verified := passed[e.ID]
		if verified == nil {
			verified = []string{}
		}
		out = append(out, PublicModel{
			ID: e.ID, ModelKey: e.ModelKey, DisplayName: e.DisplayName, Source: e.Source, Repo: e.Repo,
			Revision: e.Revision, Quant: e.Quant, WeightsBytes: e.WeightsBytes,
			MinComputeCapability: e.MinComputeCapability, ValidTP: tps, MaxModelLen: e.MaxModelLen,
			MinContextTokens: e.MinContextTokens, License: e.License, CommercialUse: e.CommercialUse,
			VerifiedGPUModels: verified,
		})
	}
	return out, nil
}

// PlanRequest deploy-plans 的查询参数。GPUMemUtilPct 须为 80–95(未提供时由 handler 填默认值 90),
// 0 等越界值一律 40001, 不在服务层静默替换为默认值。
type PlanRequest struct {
	BuyerID       int64
	OrderNo       string
	ModelID       int64
	GPUMemUtilPct int
}

// DeployPlans GET /orders/:id/deploy-plans: 对订单本人的托管 active 租约做实时可行性判定。
// 只读, 不写库; 提交部署(T-098)时服务端会重新判定, 不信任本结果。
func (s *Service) DeployPlans(ctx context.Context, req PlanRequest) (*Result, error) {
	ev, err := s.evaluateOrder(ctx, req)
	if err != nil {
		return nil, err
	}
	return ev.res, nil
}

// orderEval 一次实时判定的全部输入与结果(提交部署复用, 不信任前端传回的方案)。
type orderEval struct {
	entry *ModelEntry
	snap  *LeaseSnapshot
	res   *Result
}

func (s *Service) evaluateOrder(ctx context.Context, req PlanRequest) (*orderEval, error) {
	if !orderNoPattern.MatchString(req.OrderNo) {
		return nil, newErr(errcode.ParamInvalid, "订单号格式无效")
	}
	if req.ModelID <= 0 {
		return nil, newErr(errcode.ParamInvalid, "model_id 必填")
	}
	if req.GPUMemUtilPct < MinGPUMemUtilPct || req.GPUMemUtilPct > MaxGPUMemUtilPct {
		return nil, newErr(errcode.ParamInvalid, "gpu_mem_util 须在 0.80–0.95 之间")
	}
	// 数据源未接入时整个能力不可用, 先于查库返回, 不给出任何基于假数据的结论。
	if s.leases == nil {
		return nil, newErr(errcode.InternalError, "托管租约能力尚未上线(S2 未接入), 暂无法计算部署方案")
	}
	entry, err := s.repo.GetEntry(req.ModelID)
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.Status != StatusActive {
		return nil, newErr(errcode.NotFound, "模型不存在或未上线")
	}
	snap, err := s.leases.ManagedLeaseSnapshot(ctx, req.BuyerID, req.OrderNo, req.ModelID)
	switch {
	case errors.Is(err, ErrOrderNotFound):
		return nil, newErr(errcode.NotFound, "订单不存在")
	case errors.Is(err, ErrNotManaged):
		return nil, newErr(errcode.ParamInvalid, "该订单不是托管租用, 不支持一键部署")
	case errors.Is(err, ErrLeaseNotActive):
		return nil, newErr(errcode.Conflict, "托管租约未处于运行中, 暂不能部署")
	case err != nil:
		return nil, err
	case snap == nil:
		return nil, fmt.Errorf("lease source returned nil snapshot for order %s", req.OrderNo)
	}

	cc, _, err := s.gpus.ComputeCapabilityByModel(snap.GPUModel)
	if err != nil {
		return nil, err
	}
	passed, err := s.repo.PassedGPUModels([]int64{entry.ID})
	if err != nil {
		return nil, err
	}
	actives, err := s.repo.ListEntries(StatusActive)
	if err != nil {
		return nil, err
	}
	in, err := buildInput(entry, snap, cc, req.GPUMemUtilPct, passed[entry.ID], actives)
	if err != nil {
		return nil, err
	}
	res, err := Evaluate(in)
	if err != nil {
		return nil, err
	}
	return &orderEval{entry: entry, snap: snap, res: &res}, nil
}

// buildInput 把目录条目与租约快照组装成判定输入(纯映射, 无 I/O)。
func buildInput(e *ModelEntry, snap *LeaseSnapshot, cc string, utilPct int, verified []string, actives []ModelEntry) (Input, error) {
	tps, err := ParseValidTP(e.ValidTP)
	if err != nil {
		return Input{}, fmt.Errorf("目录条目 %d valid_tp 损坏: %w", e.ID, err)
	}
	cards := make([]Card, len(snap.Cards))
	for i, c := range snap.Cards {
		cards[i] = Card{VRAMTotalBytes: c.VRAMTotalBytes, VRAMUsedBytes: c.VRAMUsedBytes,
			Faulty: c.Faulty, FatalXidLastHour: c.FatalXidLastHour}
	}
	unknown, err := unknownInputs(snap.Unknown)
	if err != nil {
		return Input{}, err
	}
	alts := make([]Alternative, 0, len(actives))
	for _, a := range actives {
		alts = append(alts, Alternative{ModelKey: a.ModelKey, MinComputeCapability: a.MinComputeCapability})
	}
	return Input{
		Model: ModelSpec{
			ID: e.ID, ModelKey: e.ModelKey, WeightsBytes: e.WeightsBytes,
			MinComputeCapability: e.MinComputeCapability, ValidTP: tps, KVBytesPerToken: e.KVBytesPerToken,
			KVReplicatedAcrossTP: e.KVReplicatedAcrossTP, RuntimeOverheadBytes: e.RuntimeOverheadBytes,
			MaxModelLen: e.MaxModelLen, MinContextTokens: e.MinContextTokens,
			MinDriverVersion: e.MinDriverVersion, CommercialUse: e.CommercialUse,
		},
		GPUModel: snap.GPUModel, ComputeCapability: cc, DriverVersion: snap.DriverVersion,
		Cards: cards, NVLink: snap.NVLink, ModelCached: snap.ModelCached,
		CacheFreeBytes: snap.CacheFreeBytes, CacheEvictableBytes: snap.CacheEvictableBytes,
		GPUMemUtilPct: utilPct, VerifiedGPUModels: verified, Alternatives: alts,
		Unknown: unknown,
	}, nil
}

// unknownInputs 复制实现方声明的未采集输入。未知键视为实现方缺陷直接报错(按 50000 处理),
// 不静默丢弃——否则对应字段的零值会被当成真实数据参与判定。原因为空时补通用说明。
func unknownInputs(src map[string]string) (map[string]string, error) {
	if len(src) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(src))
	for k, why := range src {
		switch k {
		case InputVRAMUsage, InputModelCache, InputNVLink, InputFatalXid:
		default:
			return nil, fmt.Errorf("lease source reported unknown input key %q", k)
		}
		if why == "" {
			why = "托管租约数据源未提供该项实时数据"
		}
		out[k] = why
	}
	return out, nil
}
