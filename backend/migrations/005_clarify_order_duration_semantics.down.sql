-- 005_clarify_order_duration_semantics.down.sql
-- 回滚为 001 的原始注释。仅注释变更，无数据影响。
ALTER TABLE orders
    MODIFY COLUMN duration INT NOT NULL COMMENT '租期(小时)';
