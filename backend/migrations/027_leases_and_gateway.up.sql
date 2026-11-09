-- 027_leases_and_gateway (docs/25 §25.9, v1.2 S2 T-082)
-- 只加表加列; fleet.enabled=false 时无行为变化。down 无条件拒绝(租约/会话/补偿属业务数据)。

CREATE TABLE leases (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    lease_no VARCHAR(16) NOT NULL UNIQUE COMMENT '即网关登录名, l-xxxxxxxx',
    order_id BIGINT NOT NULL UNIQUE COMMENT '原订单; 续租沿用同一租约',
    buyer_id BIGINT NOT NULL,
    product_id BIGINT NOT NULL,
    node_id BIGINT NOT NULL,
    cards INT NOT NULL,
    delivery_kind ENUM('container','bare_metal') NOT NULL COMMENT '开通时商品 delivery_mode 快照',
    state ENUM('reserved','pending','provisioning','active','parked','suspended','grace',
               'purging','purged','failed','cancelled') NOT NULL DEFAULT 'reserved',
    upstream_port INT NULL,
    port_claim INT NULL COMMENT '占用中=upstream_port, purged/cancelled/failed 后置 NULL',
    upstream_host_key_fp VARCHAR(80) NULL COMMENT '租约容器 sshd 指纹, 网关校验',
    container_name VARCHAR(64) NULL,
    volume_name VARCHAR(64) NULL,
    access_until TIMESTAMP NULL COMMENT '网关放行截止 = 当前租期结束',
    access_changed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '撤销游标',
    grace_until TIMESTAMP NULL,
    provisioned_at TIMESTAMP NULL,
    purged_at TIMESTAMP NULL,
    fail_reason VARCHAR(256) NULL,
    resource_alert VARCHAR(128) NULL COMMENT '在租期间卡故障/节点离线, 恢复后清空',
    outage_remedy ENUM('extend','refund') NOT NULL DEFAULT 'extend' COMMENT '买家选择的停机补偿方式',
    commit_image VARCHAR(128) NULL COMMENT 'restart 换卡时 docker commit 的镜像, purge 时删除',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_node_port (node_id, port_claim),
    INDEX idx_state_access (state, access_until),
    INDEX idx_grace (state, grace_until),
    INDEX idx_access_cursor (access_changed_at, id),
    INDEX idx_buyer (buyer_id, state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE buyer_ssh_keys (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    name VARCHAR(64) NOT NULL,
    public_key VARCHAR(1024) NOT NULL COMMENT '只接受 ssh-ed25519 / ecdsa / rsa>=3072',
    fingerprint VARCHAR(80) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    revoked_at TIMESTAMP NULL,
    UNIQUE KEY uk_user_fp (user_id, fingerprint),
    INDEX idx_fp (fingerprint)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE access_sessions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    lease_id BIGINT NOT NULL,
    gw_session_id VARCHAR(64) NOT NULL UNIQUE COMMENT '网关生成, 上报幂等键',
    remote_ip VARCHAR(45) NOT NULL,
    auth_method ENUM('publickey','password') NOT NULL,
    key_fingerprint VARCHAR(80) NULL,
    started_at TIMESTAMP NOT NULL,
    ended_at TIMESTAMP NULL,
    bytes_up BIGINT NOT NULL DEFAULT 0,
    bytes_down BIGINT NOT NULL DEFAULT 0,
    end_reason VARCHAR(64) NULL,
    INDEX idx_lease_time (lease_id, started_at),
    INDEX idx_ip_time (remote_ip, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE lease_outages (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    lease_id BIGINT NOT NULL,
    order_id BIGINT NOT NULL COMMENT '停机发生时所属订单(续租后为续租单)',
    cause ENUM('node_offline','gpu_fault','container_down','full_probe_failed',
               'night_unpark_failed','provision_delay','gateway_down','wrong_freeze') NOT NULL,
    liable ENUM('supplier','platform') NOT NULL,
    started_at TIMESTAMP NOT NULL,
    ended_at TIMESTAMP NULL,
    open_claim BIGINT NULL COMMENT '未结束时=lease_id, 保证同一租约同一时刻只有一段未结束停机',
    minutes INT NULL COMMENT '结束后计算',
    remedy ENUM('extend','refund') NULL,
    remedy_applied_at TIMESTAMP NULL,
    remedy_amount BIGINT NULL COMMENT 'refund 时金额(分)',
    UNIQUE KEY uk_open (open_claim),
    INDEX idx_lease (lease_id, started_at),
    INDEX idx_order (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- payment_refunds 见 docs/25 §25.7.6 (部分退款, T-106 使用)
CREATE TABLE payment_refunds (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    payment_id BIGINT NOT NULL,
    order_no VARCHAR(32) NOT NULL,
    refund_request_id VARCHAR(64) NOT NULL UNIQUE,
    amount BIGINT NOT NULL COMMENT '分',
    kind ENUM('outage_compensation') NOT NULL COMMENT '全额退款继续走 payments.refund_status 原路径',
    source_id BIGINT NULL COMMENT 'lease_outages.id',
    status ENUM('pending','processing','success','failed') NOT NULL DEFAULT 'pending',
    yeepay_refund_no VARCHAR(64) NULL,
    last_error VARCHAR(512) NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_source (kind, source_id),
    INDEX idx_payment_status (payment_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE order_deliveries
    ADD COLUMN selftest_passed_at TIMESTAMP NULL COMMENT 'managed: 租期起算与交付证据';
