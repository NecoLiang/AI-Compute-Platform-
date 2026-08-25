-- 存证上链 worker 的重试记账 (REQ-H-021):
-- attempts 持久化重试次数, 进程重启不清零; 达到上限置 failed(死信), 修复后由
-- POST /admin/blockchain/requeue-failed 重置补推。idx_chain_status 供 worker 轮询 pending。
ALTER TABLE blockchain_attestations
    ADD COLUMN attempts INT NOT NULL DEFAULT 0 AFTER chain_status,
    ADD COLUMN last_error VARCHAR(512) NULL AFTER attempts,
    ADD INDEX idx_chain_status (chain_status);
