-- leases.suspended_at 是冻结起点: 运营认定误冻结时以它计平台责任停机(docs/25 §25.7.6), 删除会丢失在冻租约的计时依据。
-- 027 之后的迁移统一拒绝自动回退(031 纯探活遥测除外), 回滚需经审计的归档方案。
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '032 downgrade would drop lease freeze timestamps used for outage liability; automatic column removal is disabled';
