-- 028 灵活计费(docs/26 §26.8, docs/24 T-090/T-091): 零租多价格表、包月/包夜计费方式、
-- 夜间窗口与分卡列、订单计费方式快照。orders.fulfillment_mode 已由 026 加入, 此处不重复。

-- 回填前核对(§26.8): 零租商品只可能是 hourly/daily/weekly/monthly。出现 perpetual 等异常值时
-- 在任何 DDL 之前失败并报出约束名, 不静默跳过; 由人工核对后再执行。
CREATE TEMPORARY TABLE m028_precheck (
    bad_card_rental_pricing INT NOT NULL,
    CONSTRAINT m028_card_rental_unsupported_pricing_mode CHECK (bad_card_rental_pricing = 0)
);
INSERT INTO m028_precheck (bad_card_rental_pricing)
SELECT COUNT(*) FROM products
WHERE product_type='card_rental' AND pricing_mode NOT IN ('hourly','daily','weekly','monthly');
DROP TEMPORARY TABLE m028_precheck;

ALTER TABLE products
    MODIFY COLUMN pricing_mode ENUM('hourly','daily','weekly','monthly','perpetual','nightly') NOT NULL
        COMMENT '展示主价方式; card_rental 的完整价格见 product_prices',
    ADD COLUMN availability ENUM('fulltime','night_window') NOT NULL DEFAULT 'fulltime' AFTER available_hours,
    ADD COLUMN night_start_min SMALLINT NULL COMMENT '0-1439, Asia/Shanghai' AFTER availability,
    ADD COLUMN night_end_min SMALLINT NULL AFTER night_start_min,
    ADD COLUMN card_split_options VARCHAR(32) NULL COMMENT '如 "1,2,4,8"; 仅托管 container 商品' AFTER min_order,
    ADD COLUMN max_free_per_node INT NULL COMMENT '派生值, 单机最多可租卡数' AFTER stock;

CREATE TABLE product_prices (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    product_id BIGINT NOT NULL,
    pricing_mode ENUM('hourly','daily','weekly','monthly','nightly') NOT NULL,
    unit_price BIGINT NOT NULL COMMENT '分/卡/周期',
    min_duration INT NOT NULL DEFAULT 1,
    enabled TINYINT NOT NULL DEFAULT 1,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_product_mode (product_id, pricing_mode),
    INDEX idx_mode_enabled (pricing_mode, enabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 存量零租商品: 以其当前计费方式/单价/最小时长生成一行价格(当前状态可证明, 允许回填)。
-- 包含 monthly: 001 曾允许零租使用 monthly。上方核对已保证不存在其他取值。
INSERT INTO product_prices (product_id, pricing_mode, unit_price, min_duration, enabled)
SELECT id, pricing_mode, unit_price, GREATEST(COALESCE(min_duration,1),1), 1
FROM products
WHERE product_type='card_rental' AND pricing_mode IN ('hourly','daily','weekly','monthly');

-- 订单计费方式快照; 存量订单保持 NULL, 读取时回退商品当前方式(不回填无法证明的历史值)。
ALTER TABLE orders
    ADD COLUMN pricing_mode VARCHAR(20) NULL COMMENT '下单时计费方式快照; NULL=v1.2前订单' AFTER duration,
    ADD COLUMN night_start_date DATE NULL AFTER pricing_mode,
    ADD COLUMN night_start_min SMALLINT NULL AFTER night_start_date,
    ADD COLUMN night_end_min SMALLINT NULL AFTER night_start_min;
