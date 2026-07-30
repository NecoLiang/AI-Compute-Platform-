-- 002_compute_market_expansion.up.sql
-- 算力撮合交易平台 · 商品类型扩展 / 计费模式扩展 / 资源盘点 / 交付访问凭证
-- 对应需求: C-01 C-02 C-03 C-04 C-05 C-06

-- ===== C-01 商品扩展为 4 种租赁范围类型 =====
-- card_rental  零租: 按卡租, 计费 h/d/w
-- outright     零售买断: 使用权永久, 计费 perpetual
-- center       成熟算力中心: 整体打包(x卡 / x台 / 约xxxP), 计费 d/w/m/perpetual
-- colocation   空心机房: 有机房无设备, 面议

ALTER TABLE products
    ADD COLUMN product_type ENUM('card_rental','outright','center','colocation')
        NOT NULL DEFAULT 'card_rental' COMMENT '租赁范围类型' AFTER supplier_id,
    ADD COLUMN machine_count INT NULL COMMENT '台数(center/outright 用)' AFTER card_count,
    ADD COLUMN total_pflops_approx VARCHAR(32) NULL COMMENT '约算力如"128P", 供给方自填参考值, 平台不做换算不校验' AFTER machine_count,
    ADD COLUMN power_capacity_kw INT NULL COMMENT '电力容量 kW(colocation 用)' AFTER total_pflops_approx,
    ADD COLUMN rack_count INT NULL COMMENT '机柜数(colocation 用)' AFTER power_capacity_kw,
    ADD COLUMN price_negotiable TINYINT NOT NULL DEFAULT 0 COMMENT '1=面议(colocation 用)' AFTER unit_price;

-- colocation 空心机房没有卡也没有交付方式, 三个原 NOT NULL 字段必须放开为可空
ALTER TABLE products
    MODIFY COLUMN gpu_model VARCHAR(64) NULL COMMENT 'colocation 无卡时为 NULL',
    MODIFY COLUMN card_count INT NULL COMMENT 'colocation 无卡时为 NULL',
    MODIFY COLUMN delivery_mode ENUM('bare_metal','container','vm','rack') NULL COMMENT 'colocation 无设备时为 NULL';

-- ===== C-04 计费模式扩展: 增加 daily / perpetual =====
-- ENUM 加值属于加宽操作, 存量行取值不受影响
ALTER TABLE products
    MODIFY COLUMN pricing_mode ENUM('hourly','daily','weekly','monthly','perpetual') NOT NULL;

-- 供给方工作台按类型分组查询用(C-03)
ALTER TABLE products
    ADD INDEX idx_supplier_type (supplier_id, product_type, status);

-- ===== C-05 资源同步与盘点(主动盘/被动报) =====
CREATE TABLE resource_snapshots (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    product_id BIGINT NOT NULL,
    supplier_id BIGINT NOT NULL,
    sync_type ENUM('active','passive') NOT NULL COMMENT 'active=平台主动盘点 passive=机房主动上报',
    stock_before INT NOT NULL,
    stock_after INT NOT NULL,
    diff INT NOT NULL,
    reason VARCHAR(256),
    operator_id BIGINT NOT NULL,
    anomaly TINYINT DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_product (product_id, created_at),
    INDEX idx_anomaly (anomaly, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ===== C-06 交付访问凭证 =====
ALTER TABLE order_deliveries
    ADD COLUMN access_key VARCHAR(64) NULL COMMENT '访问凭证标识 ak-+32位hex' AFTER credential_encrypted,
    ADD COLUMN access_value_encrypted TEXT NULL COMMENT 'AES-256-GCM 加密的访问凭证明文, 禁止存明文' AFTER access_key,
    ADD COLUMN access_status ENUM('none','generated','delivered','revoked') NOT NULL DEFAULT 'none' AFTER access_value_encrypted,
    ADD COLUMN access_expires_at TIMESTAMP NULL AFTER access_status,
    ADD COLUMN revoked_at TIMESTAMP NULL AFTER access_expires_at,
    ADD UNIQUE KEY uk_access_key (access_key);
