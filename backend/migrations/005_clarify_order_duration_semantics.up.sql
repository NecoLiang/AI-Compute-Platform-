-- 005_clarify_order_duration_semantics.up.sql
-- 明确 orders.duration 的语义为「计费周期数」而非「小时数」(C-04 口径确认)。
--
-- 背景: 001 原注释为 '租期(小时)', 但 CalcOrderAmount 把 duration 当纯乘数使用,
-- 且 unit_price 是「元/卡·计费周期」。若前端按小时传, daily 商品会被按天单价 ×24 倍多收费。
-- 现统一口径: hourly=小时数, daily=天数, weekly=周数, monthly=月数, perpetual 强制 1。
--
-- 本迁移只改列注释, 不改数据与类型, 可安全重复执行。
-- 001 中的注释已同步修正(供全新安装使用)。
ALTER TABLE orders
    MODIFY COLUMN duration INT NOT NULL
    COMMENT '计费周期数: hourly=小时 daily=天 weekly=周 monthly=月 perpetual=1';
