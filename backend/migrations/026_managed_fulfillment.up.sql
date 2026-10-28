-- 026 托管履约(docs/25 §25.9, docs/24 S1): 商品/订单履约模式、订单分账就绪时点、
-- 托管节点管理列、卡槽、探活报告、节点任务队列。只加表加列, 默认值不改变存量行为。
ALTER TABLE products
    ADD COLUMN fulfillment_mode ENUM('manual','managed') NOT NULL DEFAULT 'manual'
        COMMENT '履约模式: manual=供给方人工交付(C-06) managed=平台托管开通(C-09)' AFTER delivery_mode;

ALTER TABLE orders
    ADD COLUMN fulfillment_mode ENUM('manual','managed') NOT NULL DEFAULT 'manual'
        COMMENT '下单时商品履约模式快照' AFTER product_id,
    ADD COLUMN settle_ready_at TIMESTAMP NULL
        COMMENT 'managed: 租期结束且停机结算完成后写入, 非 NULL 才允许分账(D-10)';

ALTER TABLE supplier_nodes
    MODIFY COLUMN node_key_hash CHAR(64) NULL COMMENT 'push 模式节点密钥哈希; ssh 模式为 NULL',
    ADD COLUMN mgmt_mode ENUM('push','ssh') NOT NULL DEFAULT 'push' COMMENT 'push=docs/23心跳 ssh=平台SSH管理',
    ADD COLUMN ssh_host VARCHAR(255) NULL,
    ADD COLUMN ssh_port INT NULL,
    ADD COLUMN ssh_user VARCHAR(64) NULL,
    ADD COLUMN mgmt_pubkey VARCHAR(255) NULL COMMENT '平台管理公钥(按节点生成)',
    ADD COLUMN mgmt_privkey_encrypted TEXT NULL COMMENT 'AES-256-GCM, security.credential_key',
    ADD COLUMN host_key_fingerprint VARCHAR(80) NULL COMMENT 'SHA256:..., 供给方确认后钉死',
    ADD COLUMN host_key_confirmed_at TIMESTAMP NULL,
    ADD COLUMN host_fp_claim VARCHAR(80) NULL COMMENT 'verified 时 = host_key_fingerprint, 否则 NULL; 防一机多挂',
    ADD COLUMN verify_status ENUM('unverified','verifying','verified','failed','key_mismatch','needs_reimage','suspended')
        NOT NULL DEFAULT 'unverified',
    ADD COLUMN schedulable TINYINT NOT NULL DEFAULT 1 COMMENT '0=排空, 不接新分配',
    ADD COLUMN verified_at TIMESTAMP NULL,
    ADD COLUMN last_probe_ok_at TIMESTAMP NULL,
    ADD COLUMN probe_fail_count INT NOT NULL DEFAULT 0,
    ADD COLUMN cpu_cores INT NULL,
    ADD COLUMN mem_gb INT NULL,
    ADD COLUMN disk_free_gb INT NULL,
    ADD COLUMN disk_per_card_gb INT NULL COMMENT '供给方填写, 探活核实',
    ADD COLUMN disk_quota_supported TINYINT NULL,
    ADD COLUMN driver_version VARCHAR(32) NULL,
    ADD COLUMN cuda_version VARCHAR(16) NULL,
    ADD COLUMN lease_port_start INT NULL,
    ADD COLUMN lease_port_end INT NULL,
    ADD INDEX idx_verify (verify_status, schedulable),
    ADD UNIQUE KEY uk_host_fp_claim (host_fp_claim);

CREATE TABLE gpu_slots (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    node_id BIGINT NOT NULL,
    gpu_index INT NOT NULL,
    gpu_uuid VARCHAR(64) NOT NULL COMMENT '全局唯一: 同一张物理卡不能重复上架',
    pci_bus_id VARCHAR(32) NOT NULL,
    vram_mb INT NOT NULL,
    topo_group VARCHAR(16) NOT NULL COMMENT '同组=NVLink/同PCIe交换/同NUMA, 由 nvidia-smi topo 解析',
    state ENUM('free','reserved','leased','draining','faulty','withheld') NOT NULL DEFAULT 'free',
    lease_id BIGINT NULL,
    state_changed_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_node_index (node_id, gpu_index),
    UNIQUE KEY uk_gpu_uuid (gpu_uuid),
    INDEX idx_node_state (node_id, state),
    INDEX idx_lease (lease_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE node_probe_reports (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    node_id BIGINT NOT NULL,
    kind ENUM('verify','port_recheck') NOT NULL,
    result ENUM('pass','fail') NOT NULL,
    checks JSON NOT NULL COMMENT '[{id:P1,ok,detail}], 不含任何凭证',
    inventory JSON NULL COMMENT 'GPU/拓扑/CPU/内存/磁盘/OS/驱动',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_node_time (node_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- DB 行即队列, 与区块链 worker 同一模式
CREATE TABLE node_tasks (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    node_id BIGINT NOT NULL,
    lease_id BIGINT NULL,
    deployment_id BIGINT NULL COMMENT 'docs/27',
    kind ENUM('probe','bootstrap_key','apply_firewall','provision','selftest','park','unpark',
              'kill_sessions','stop','purge','pull_model','deploy_model','stop_model') NOT NULL,
    state ENUM('pending','running','succeeded','failed','dead') NOT NULL DEFAULT 'pending',
    priority TINYINT NOT NULL DEFAULT 5 COMMENT '小者优先: stop/kill_sessions=1, provision=3, purge=8',
    attempts INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 3,
    run_after TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    locked_until TIMESTAMP NULL,
    payload JSON NULL,
    result JSON NULL,
    last_error VARCHAR(512) NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_claim (state, run_after, priority),
    INDEX idx_node_state (node_id, state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
