-- T-088: 冻结时刻(租约 active/parked → suspended)。解冻经核查为误冻结时, 冻结时长按 docs/25 §25.7.6
-- 计入平台责任停机; 以本列为起点, 不依赖会被其他操作刷新的 access_changed_at。解冻/离开 suspended 时清空。
ALTER TABLE leases ADD COLUMN suspended_at DATETIME NULL AFTER grace_until;
