-- 001_initial_schema.up.sql
-- 算力撮合交易平台 · 初始数据库

CREATE TABLE users (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    phone VARCHAR(20) NOT NULL UNIQUE,
    email VARCHAR(128) DEFAULT '',
    password_hash VARCHAR(256) NOT NULL,
    status ENUM('active','frozen') DEFAULT 'active',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_phone (phone)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE user_sessions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    token_hash VARCHAR(128) NOT NULL,
    ip VARCHAR(45),
    device VARCHAR(256),
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_user (user_id),
    INDEX idx_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE user_kyc (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL UNIQUE,
    real_name VARCHAR(64) NOT NULL,
    id_card VARCHAR(32) NOT NULL,
    status ENUM('pending','verified','rejected') DEFAULT 'pending',
    rejected_reason VARCHAR(256),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE enterprises (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL UNIQUE,
    name VARCHAR(128) NOT NULL,
    uscc VARCHAR(32) NOT NULL,
    license_url VARCHAR(512),
    legal_person VARCHAR(64),
    status ENUM('pending','verified','rejected') DEFAULT 'pending',
    rejected_reason VARCHAR(256),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE user_roles (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    role ENUM('buyer','supplier','vendor','funder','operator','admin') NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_user_role (user_id, role),
    INDEX idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE supplier_qualifications (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    qual_type VARCHAR(64) NOT NULL COMMENT 'idc/cert',
    cert_name VARCHAR(128) NOT NULL,
    cert_number VARCHAR(64),
    cert_url VARCHAR(512),
    expires_at DATE,
    status ENUM('pending','verified','rejected','expired') DEFAULT 'pending',
    rejected_reason VARCHAR(256),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE products (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    supplier_id BIGINT NOT NULL,
    gpu_model VARCHAR(64) NOT NULL,
    card_count INT NOT NULL,
    cpu_spec VARCHAR(128),
    memory_spec VARCHAR(64),
    storage_spec VARCHAR(64),
    bandwidth_spec VARCHAR(32),
    delivery_mode ENUM('bare_metal','container','vm','rack') NOT NULL,
    pricing_mode ENUM('hourly','weekly','monthly') NOT NULL,
    unit_price BIGINT NOT NULL COMMENT '单价(分)/卡·时',
    available_hours VARCHAR(64) COMMENT '全天/22:00-08:00',
    stock INT NOT NULL,
    min_order INT DEFAULT 1,
    min_duration INT DEFAULT 1,
    region VARCHAR(32),
    status ENUM('draft','pending','active','sold_out','offline','frozen') DEFAULT 'draft',
    self_operated TINYINT DEFAULT 0,
    compliance_agreed TINYINT DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_search (gpu_model, region, pricing_mode, status),
    INDEX idx_supplier (supplier_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE orders (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    order_no VARCHAR(32) NOT NULL UNIQUE,
    buyer_id BIGINT NOT NULL,
    product_id BIGINT NOT NULL,
    quantity INT NOT NULL,
    duration INT NOT NULL COMMENT '计费周期数: hourly=小时 daily=天 weekly=周 monthly=月 perpetual=1',
    unit_price BIGINT NOT NULL COMMENT '单价(分)',
    total_amount BIGINT NOT NULL COMMENT '总金额(分)',
    platform_fee BIGINT NOT NULL COMMENT '平台佣金(分)',
    status ENUM('pending_payment','paid','provisioning','active','completed','cancelled','refunding','refunded','frozen') DEFAULT 'pending_payment',
    payment_expires_at TIMESTAMP NULL,
    lease_start_at TIMESTAMP NULL,
    lease_end_at TIMESTAMP NULL,
    compliance_agreed TINYINT DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_buyer (buyer_id, status),
    INDEX idx_status (status, payment_expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE order_deliveries (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    order_id BIGINT NOT NULL UNIQUE,
    credential_encrypted TEXT COMMENT 'AES-256加密的交付凭证',
    confirmed_by_buyer TINYINT DEFAULT 0,
    buyer_confirmed_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE payments (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    order_no VARCHAR(32) NOT NULL,
    amount BIGINT NOT NULL COMMENT '支付金额(分)',
    channel VARCHAR(16) COMMENT 'wechat/alipay/bank',
    yeepay_tx_id VARCHAR(64),
    status ENUM('pending','paid','failed','refunded') DEFAULT 'pending',
    paid_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_yeepay_tx (yeepay_tx_id),
    INDEX idx_order (order_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE settlements (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    settlement_id VARCHAR(64) NOT NULL UNIQUE,
    order_no VARCHAR(32) NOT NULL,
    payee_type VARCHAR(16) COMMENT 'supplier/platform',
    payee_id BIGINT,
    amount BIGINT NOT NULL COMMENT '分账金额(分)',
    status ENUM('pending','processing','success','failed') DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_order (order_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE credit_scores (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    supplier_id BIGINT NOT NULL UNIQUE,
    fulfill_rate DECIMAL(5,2) DEFAULT 100.00,
    sla_rate DECIMAL(5,2) DEFAULT 100.00,
    violation_count INT DEFAULT 0,
    total_orders INT DEFAULT 0,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE leads (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    type ENUM('equipment','construction','finance_lease') NOT NULL,
    contact_name VARCHAR(64),
    contact_phone VARCHAR(20),
    contact_email VARCHAR(128),
    description TEXT,
    amount_range VARCHAR(32),
    term VARCHAR(32),
    status ENUM('new','assigned','following','quoted','closed','cancelled') DEFAULT 'new',
    assignee_id BIGINT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_status (status),
    INDEX idx_assignee (assignee_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE lead_followups (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    lead_id BIGINT NOT NULL,
    operator_id BIGINT NOT NULL,
    content TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_lead (lead_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE commissions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    lead_id BIGINT NOT NULL,
    deal_amount BIGINT NOT NULL COMMENT '成交金额(分)',
    commission_rate DECIMAL(5,2) NOT NULL,
    commission_amount BIGINT NOT NULL COMMENT '佣金(分)',
    status ENUM('pending','settled') DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE risk_alerts (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    level ENUM('high','medium','low') NOT NULL,
    alert_type VARCHAR(32) NOT NULL,
    target_type VARCHAR(32),
    target_id BIGINT,
    rule_detail TEXT,
    status ENUM('pending','processing','resolved','dismissed') DEFAULT 'pending',
    operator_id BIGINT,
    resolution TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE audit_logs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    operator_id BIGINT,
    action VARCHAR(64) NOT NULL,
    target_type VARCHAR(32),
    target_id BIGINT,
    before_value TEXT,
    after_value TEXT,
    ip VARCHAR(45),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_target (target_type, target_id),
    INDEX idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE blockchain_attestations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    target_type VARCHAR(32) NOT NULL,
    target_id VARCHAR(64) NOT NULL,
    data_hash VARCHAR(128) NOT NULL,
    signers JSON,
    chain_tx_id VARCHAR(128),
    chain_status ENUM('pending','confirmed','failed') DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    confirmed_at TIMESTAMP NULL,
    INDEX idx_target (target_type, target_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Seed: default admin user (password: admin123)
INSERT INTO users (phone, password_hash, status) VALUES ('admin', '$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy', 'active');
INSERT INTO user_roles (user_id, role) VALUES (1, 'buyer'), (1, 'operator'), (1, 'admin');
