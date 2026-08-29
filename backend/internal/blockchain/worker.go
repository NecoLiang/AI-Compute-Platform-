package blockchain

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	// workerInterval 上链是"几秒~十几秒"量级的异步旁路 (REQ-H-020), 30s 轮询足够。
	workerInterval = 30 * time.Second
	// workerMaxAttempts 首次 + 重试 3 次 = 4 次后进死信 (REQ-H-021)。
	workerMaxAttempts = 4
	workerBatchSize   = 100
)

// RunWorker 存证上链 worker (T-058)。轮询 pending 存证行推给 BSN, 成功回写 TX ID
// (REQ-H-022), 失败记次数、耗尽进死信。以 DB 行为队列, 见 Attestation 的注释。
// BSN 未配置时空转等待 —— 存证照常积累为 pending, 配置上线重启后自动补推。
// 阻塞运行, 应以 goroutine 启动; ctx 取消后返回。
func (s *Service) RunWorker(ctx context.Context) {
	if !s.bsn.Configured() {
		slog.Warn("存证上链 worker 待命: BSN 未配置, 存证事件将以 pending 积累, 配置后自动补推",
			"缺少", "blockchain.gateway_url / api_key / contract_key")
	}
	ticker := time.NewTicker(workerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.bsn.Configured() {
				continue
			}
			s.processPendingOnce(ctx)
		}
	}
}

// processPendingOnce 处理一批 pending 存证。单条失败不中断整批。
func (s *Service) processPendingOnce(ctx context.Context) {
	list, err := s.repo.ListPending(workerBatchSize)
	if err != nil {
		slog.Error("读取待上链存证失败", "error", err)
		return
	}
	for _, att := range list {
		if ctx.Err() != nil {
			return
		}
		txID, err := s.bsn.UploadHash(ctx, att.DataHash)
		if err != nil {
			// 链账户未开户是基础设施级状态: 整批等待, 不消耗单条存证的重试次数。
			if errors.Is(err, ErrChainAccountMissing) {
				slog.Warn("存证上链暂缓: 链账户未在链上开户, 待 BSN 门户绑定/充能量值后自动恢复", "error", err)
				return
			}
			slog.Error("存证上链失败", "id", att.ID, "target", att.TargetType+"/"+att.TargetID,
				"attempt", att.Attempts+1, "max", workerMaxAttempts, "error", err)
			if rerr := s.repo.RecordFailure(att.ID, err.Error(), workerMaxAttempts); rerr != nil {
				slog.Error("记录上链失败状态失败", "id", att.ID, "error", rerr)
			}
			continue
		}
		if err := s.repo.MarkConfirmed(att.ID, txID); err != nil {
			slog.Error("回写 TX ID 失败", "id", att.ID, "tx_id", txID, "error", err)
			continue
		}
		slog.Info("存证已上链", "id", att.ID, "target", att.TargetType+"/"+att.TargetID, "tx_id", txID)
	}
}
