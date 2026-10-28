-- 026 回退只删除本迁移新增的表与列(docs/25 §25.9), 绝不删除业务行。
-- 已存在托管数据(ssh 节点 / managed 商品或订单 / 已写分账就绪时点)时拒绝回退:
-- 这些行无法在旧结构中表达(ssh 节点 node_key_hash 为 NULL, 恢复 NOT NULL 会失败或丢数据),
-- 须先由运营按审计流程处置后再回退。拒绝时 golang-migrate 记为 dirty(版本 25),
-- 处置完成后 `migrate force 26` 再重新 down; 残留的守卫过程会在下次执行时先被 DROP。
DROP PROCEDURE IF EXISTS tf_026_down_guard;
CREATE PROCEDURE tf_026_down_guard()
BEGIN
    IF EXISTS (SELECT 1 FROM supplier_nodes WHERE mgmt_mode = 'ssh' OR node_key_hash IS NULL)
       OR EXISTS (SELECT 1 FROM products WHERE fulfillment_mode = 'managed')
       OR EXISTS (SELECT 1 FROM orders WHERE fulfillment_mode = 'managed' OR settle_ready_at IS NOT NULL) THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '026 downgrade refused: ssh-mode nodes or managed products/orders exist; archive them via audited process first';
    END IF;
END;
CALL tf_026_down_guard();
DROP PROCEDURE IF EXISTS tf_026_down_guard;

DROP TABLE IF EXISTS node_tasks;
DROP TABLE IF EXISTS node_probe_reports;
DROP TABLE IF EXISTS gpu_slots;

ALTER TABLE gpu_catalog
    DROP COLUMN probe_baseline_tflops;

ALTER TABLE supplier_nodes
    DROP INDEX uk_host_fp_claim,
    DROP INDEX idx_verify,
    DROP COLUMN mgmt_mode,
    DROP COLUMN ssh_host,
    DROP COLUMN ssh_port,
    DROP COLUMN ssh_user,
    DROP COLUMN mgmt_pubkey,
    DROP COLUMN mgmt_privkey_encrypted,
    DROP COLUMN host_key_fingerprint,
    DROP COLUMN host_key_confirmed_at,
    DROP COLUMN host_fp_claim,
    DROP COLUMN verify_status,
    DROP COLUMN schedulable,
    DROP COLUMN verified_at,
    DROP COLUMN last_probe_ok_at,
    DROP COLUMN probe_fail_count,
    DROP COLUMN cpu_cores,
    DROP COLUMN mem_gb,
    DROP COLUMN disk_free_gb,
    DROP COLUMN disk_per_card_gb,
    DROP COLUMN disk_quota_supported,
    DROP COLUMN driver_version,
    DROP COLUMN cuda_version,
    DROP COLUMN lease_port_start,
    DROP COLUMN lease_port_end,
    MODIFY COLUMN node_key_hash CHAR(64) NOT NULL;

ALTER TABLE orders
    DROP COLUMN settle_ready_at,
    DROP COLUMN fulfillment_mode;

ALTER TABLE products
    DROP COLUMN fulfillment_mode;
