-- 003_equipment_and_collateral.up.sql
-- C-08 设备商品发布（一手/二手）+ 询价
-- C-07 中登网动产融资登记信息（运营人工录入 + 查询展示）

-- ===== C-08 设备商品 =====
-- 注意: `condition` 是 MySQL 保留字, 字段名用 condition_type
CREATE TABLE equipment_products (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    vendor_id BIGINT NOT NULL,
    title VARCHAR(128) NOT NULL,
    equipment_type ENUM('gpu_server','storage','network','cooling','ups','rack','other') NOT NULL,
    brand VARCHAR(64),
    model VARCHAR(64),
    condition_type ENUM('new','used') NOT NULL COMMENT 'new=一手 used=二手',
    manufacture_year INT COMMENT '出厂年份(二手用)',
    usage_desc VARCHAR(256) COMMENT '使用情况(二手用)',
    quantity INT NOT NULL,
    unit_price BIGINT DEFAULT 0 COMMENT '单价(分)',
    price_negotiable TINYINT DEFAULT 0 COMMENT '面议',
    region VARCHAR(32),
    description TEXT,
    images JSON,
    status ENUM('draft','pending','active','sold_out','offline') DEFAULT 'draft',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_search (equipment_type, condition_type, region, status),
    INDEX idx_vendor (vendor_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 设备不接在线支付(金额大/需线下验货议价), 只做询价撮合转线索
CREATE TABLE equipment_inquiries (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    equipment_id BIGINT NOT NULL,
    buyer_id BIGINT NOT NULL,
    quantity INT NOT NULL,
    contact_name VARCHAR(64) NOT NULL,
    contact_phone VARCHAR(20) NOT NULL,
    message TEXT,
    status ENUM('new','replied','closed') DEFAULT 'new',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_equipment (equipment_id, status),
    INDEX idx_buyer (buyer_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ===== C-07 中登网动产融资登记 =====
-- 数据来源说明: 中国人民银行征信中心动产融资统一登记公示系统(中登网)不提供对外数据接口,
-- 因此本表数据只能由平台运营人工查询官方系统后录入(data_source 固定 manual)。
-- 所有对外查询响应必须携带合规声明, 不得暗示平台与官方系统有数据直连。
CREATE TABLE collateral_registrations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    reg_no VARCHAR(64) NOT NULL COMMENT '中登网登记编号',
    reg_type ENUM('finance_lease','mortgage','factoring','other') NOT NULL COMMENT '登记类型',
    lessor_name VARCHAR(128) NOT NULL COMMENT '出租人/权利人',
    lessee_name VARCHAR(128) NOT NULL COMMENT '承租人/义务人',
    lessee_uscc VARCHAR(32) COMMENT '承租人统一社会信用代码',
    collateral_desc TEXT COMMENT '标的物描述',
    reg_start_date DATE,
    reg_end_date DATE,
    status ENUM('valid','expired','cancelled') DEFAULT 'valid',
    data_source ENUM('manual') DEFAULT 'manual' COMMENT 'v1仅支持运营人工录入(中登网无对外数据接口)',
    source_note VARCHAR(256) COMMENT '录入依据,如查询截图编号/查询日期',
    verified_at DATE COMMENT '人工核验日期',
    created_by BIGINT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_reg_no (reg_no),
    INDEX idx_lessee (lessee_name),
    INDEX idx_uscc (lessee_uscc)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
