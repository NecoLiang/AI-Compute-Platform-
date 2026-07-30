-- 002_compute_market_expansion.down.sql
-- 回滚 002. 顺序与 up 相反。
-- 注意: 回滚是有损操作 —— 002 之后新增的 daily/perpetual 计费商品、colocation 商品、
-- 全部盘点快照、全部访问凭证都会被降级或删除。生产环境执行前务必先备份。

-- ===== C-06 回滚 =====
ALTER TABLE order_deliveries
    DROP INDEX uk_access_key,
    DROP COLUMN revoked_at,
    DROP COLUMN access_expires_at,
    DROP COLUMN access_status,
    DROP COLUMN access_value_encrypted,
    DROP COLUMN access_key;

-- ===== C-05 回滚 =====
DROP TABLE IF EXISTS resource_snapshots;

-- ===== C-03 索引回滚 =====
ALTER TABLE products DROP INDEX idx_supplier_type;

-- ===== C-04 回滚 =====
-- 收窄 ENUM 前必须先把新增取值映射回旧取值, 否则 MODIFY 会因非法值失败(严格模式)或被静默置空
UPDATE products SET pricing_mode = 'hourly'  WHERE pricing_mode = 'daily';
UPDATE products SET pricing_mode = 'monthly' WHERE pricing_mode = 'perpetual';
ALTER TABLE products
    MODIFY COLUMN pricing_mode ENUM('hourly','weekly','monthly') NOT NULL;

-- ===== C-01 回滚 =====
-- 恢复 NOT NULL 前必须先补齐 NULL 值(colocation 商品原本三列为 NULL)
UPDATE products SET gpu_model = ''           WHERE gpu_model IS NULL;
UPDATE products SET card_count = 0           WHERE card_count IS NULL;
UPDATE products SET delivery_mode = 'rack'   WHERE delivery_mode IS NULL;
ALTER TABLE products
    MODIFY COLUMN gpu_model VARCHAR(64) NOT NULL,
    MODIFY COLUMN card_count INT NOT NULL,
    MODIFY COLUMN delivery_mode ENUM('bare_metal','container','vm','rack') NOT NULL;

ALTER TABLE products
    DROP COLUMN price_negotiable,
    DROP COLUMN rack_count,
    DROP COLUMN power_capacity_kw,
    DROP COLUMN total_pflops_approx,
    DROP COLUMN machine_count,
    DROP COLUMN product_type;
