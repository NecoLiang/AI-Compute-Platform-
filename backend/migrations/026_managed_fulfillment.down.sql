-- 026 只加表加列; 回退仅删除本迁移新增的表与列(docs/25 §25.9)。
-- ssh 模式节点没有 node_key_hash, 恢复 NOT NULL 前必须先删掉这些行(其卡槽/探活/任务表已整体删除)。
DROP TABLE IF EXISTS node_tasks;
DROP TABLE IF EXISTS node_probe_reports;
DROP TABLE IF EXISTS gpu_slots;

DELETE FROM supplier_nodes WHERE node_key_hash IS NULL;

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
