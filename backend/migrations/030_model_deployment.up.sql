-- 030 模型一键部署(docs/27 §27.7, docs/24 S5): GPU 型号算力架构、节点模型缓存容量、
-- 模型目录与真机验证记录、节点权重缓存、部署记录、健康采样。只加表加列, 目录为空时功能对买家不可见。
ALTER TABLE gpu_catalog
    ADD COLUMN compute_capability VARCHAR(8) NULL COMMENT '如 8.0 / 8.9 / 9.0; 未填则该型号不放行一键部署' AFTER interconnect;

ALTER TABLE supplier_nodes
    ADD COLUMN model_cache_gb INT NULL COMMENT '模型缓存区容量, 供给方声明, 探活核实';

CREATE TABLE model_catalog (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    model_key VARCHAR(64) NOT NULL UNIQUE,
    display_name VARCHAR(128) NOT NULL,
    source ENUM('modelscope') NOT NULL DEFAULT 'modelscope',
    repo VARCHAR(255) NOT NULL,
    revision VARCHAR(64) NOT NULL COMMENT '固定提交号',
    manifest JSON NOT NULL COMMENT '[{path,size,sha256}]',
    weights_bytes BIGINT NOT NULL,
    quant VARCHAR(16) NOT NULL,
    min_compute_capability VARCHAR(8) NOT NULL,
    valid_tp VARCHAR(32) NOT NULL,
    kv_bytes_per_token BIGINT NOT NULL,
    kv_replicated_across_tp TINYINT NOT NULL DEFAULT 0,
    runtime_overhead_bytes BIGINT NOT NULL,
    max_model_len INT NOT NULL,
    min_context_tokens INT NOT NULL DEFAULT 8192,
    engine ENUM('vllm') NOT NULL DEFAULT 'vllm',
    engine_image VARCHAR(255) NOT NULL COMMENT 'ref@sha256',
    min_driver_version VARCHAR(32) NOT NULL,
    engine_args JSON NOT NULL,
    metrics_map JSON NOT NULL,
    startup_timeout_s INT NOT NULL DEFAULT 1200,
    license VARCHAR(64) NOT NULL,
    commercial_use ENUM('allowed','restricted','unknown') NOT NULL,
    status ENUM('draft','active','retired') NOT NULL DEFAULT 'draft',
    created_by BIGINT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 真机验证记录: 条目上线前至少一条 result=pass
CREATE TABLE model_catalog_verifications (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    model_id BIGINT NOT NULL,
    gpu_model VARCHAR(64) NOT NULL,
    cards INT NOT NULL,
    tp INT NOT NULL,
    max_model_len INT NOT NULL,
    measured_overhead_bytes BIGINT NULL,
    result ENUM('pass','fail') NOT NULL,
    notes VARCHAR(512) NULL,
    tested_by BIGINT NOT NULL,
    tested_at TIMESTAMP NOT NULL,
    INDEX idx_model (model_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE node_model_cache (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    node_id BIGINT NOT NULL,
    model_id BIGINT NOT NULL,
    state ENUM('downloading','ready','failed','evicting') NOT NULL,
    bytes_done BIGINT NOT NULL DEFAULT 0,
    verified_at TIMESTAMP NULL,
    last_used_at TIMESTAMP NULL,
    last_error VARCHAR(512) NULL,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_node_model (node_id, model_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE model_deployments (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    deploy_no VARCHAR(32) NOT NULL UNIQUE,
    lease_id BIGINT NOT NULL,
    order_id BIGINT NOT NULL,
    buyer_id BIGINT NOT NULL,
    node_id BIGINT NOT NULL,
    model_id BIGINT NOT NULL,
    gpu_uuids JSON NOT NULL,
    tp INT NOT NULL,
    dp INT NOT NULL,
    max_model_len INT NOT NULL,
    gpu_mem_util DECIMAL(3,2) NOT NULL,
    engine_port INT NULL,
    engine_port_claim INT NULL COMMENT '运行期间=engine_port, 停止后 NULL',
    lease_active_claim BIGINT NULL COMMENT '未终止时=lease_id, 保证每租约最多一个部署',
    container_name VARCHAR(64) NULL,
    state ENUM('pending','checking','pulling','starting','running','restarting',
               'stopping','stopped','failed') NOT NULL DEFAULT 'pending',
    health ENUM('unknown','healthy','degraded','unhealthy') NOT NULL DEFAULT 'unknown',
    health_reason VARCHAR(64) NULL,
    canary_enabled TINYINT NOT NULL DEFAULT 1,
    auto_resume TINYINT NOT NULL DEFAULT 1,
    restart_count INT NOT NULL DEFAULT 0,
    last_restart_at TIMESTAMP NULL,
    baseline_canary_ms INT NULL,
    feasibility JSON NOT NULL COMMENT '提交时的判定明细',
    license_ack_at TIMESTAMP NULL,
    untested_ack_at TIMESTAMP NULL COMMENT 'F8: 卡型未实测时买家确认时间',
    fail_reason VARCHAR(512) NULL,
    log_tail TEXT NULL COMMENT '失败时引擎日志末尾 200 行',
    started_at TIMESTAMP NULL,
    stopped_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_lease_active (lease_active_claim),
    UNIQUE KEY uk_node_engine_port (node_id, engine_port_claim),
    INDEX idx_state_health (state, health),
    INDEX idx_buyer (buyer_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE deployment_health_samples (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    deployment_id BIGINT NOT NULL,
    sampled_at TIMESTAMP NOT NULL,
    container_up TINYINT NOT NULL,
    health_ok TINYINT NULL,
    models_ok TINYINT NULL,
    canary_ok TINYINT NULL,
    canary_ms INT NULL,
    requests_running INT NULL,
    requests_waiting INT NULL,
    kv_usage_pct DECIMAL(5,2) NULL,
    ttft_p50_ms INT NULL,
    ttft_p95_ms INT NULL,
    gen_tokens_per_s DECIMAL(10,2) NULL,
    prompt_tokens_per_s DECIMAL(10,2) NULL,
    preemptions INT NULL,
    gpu_temp_max_c SMALLINT NULL,
    throttled TINYINT NULL,
    health ENUM('unknown','healthy','degraded','unhealthy') NOT NULL,
    INDEX idx_dep_time (deployment_id, sampled_at),
    INDEX idx_time (sampled_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
