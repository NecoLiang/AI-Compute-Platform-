-- 008_invoice_management.up.sql
-- 发票管理: 买家开票信息(抬头)、发票记录与发票-订单关联
--
-- 口径:
--   - 平台统一开票, 金额 = 关联订单实付 total_amount 合计(分)。
--   - 一张发票可合并多个订单; 一个订单只能被一张 pending/issued 发票占用
--     (invoice_orders.order_id 唯一约束兜底, 被驳回的发票不占用订单)。
--   - 抬头冗余快照进 invoices, 之后修改 invoice_titles 不影响历史发票。
--   - PDF 存 MEDIUMBLOB: 早期量级小(单张 <1MB)且后端尚无文件存储设施, 后续可迁 OSS。
--   - 红冲(red_flushed)与个人普票(title_type='personal')仅预留枚举, 本迭代不实现流程。

CREATE TABLE invoice_titles (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    buyer_id BIGINT NOT NULL UNIQUE COMMENT '每买家一份抬头',
    title_type VARCHAR(16) NOT NULL DEFAULT 'enterprise' COMMENT 'enterprise=企业, personal 预留',
    company_name VARCHAR(128) NOT NULL COMMENT '企业名称',
    tax_no VARCHAR(32) NOT NULL COMMENT '纳税人识别号/统一社会信用代码',
    bank_name VARCHAR(128) NOT NULL COMMENT '开户行',
    bank_account VARCHAR(64) NOT NULL COMMENT '银行账号',
    address VARCHAR(255) NULL COMMENT '注册地址(专票预留)',
    phone VARCHAR(32) NULL COMMENT '注册电话(专票预留)',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE invoices (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    invoice_no VARCHAR(32) NOT NULL UNIQUE COMMENT '系统编号 INV-YYYY-NNNN',
    buyer_id BIGINT NOT NULL,
    company_name VARCHAR(128) NOT NULL COMMENT '抬头快照',
    tax_no VARCHAR(32) NOT NULL COMMENT '抬头快照',
    bank_name VARCHAR(128) NOT NULL COMMENT '抬头快照',
    bank_account VARCHAR(64) NOT NULL COMMENT '抬头快照',
    amount_fen BIGINT NOT NULL COMMENT '开票金额(分)=关联订单实付合计',
    invoice_type VARCHAR(16) NOT NULL DEFAULT 'vat_special' COMMENT 'vat_special=增值税专用发票',
    status ENUM('pending','issued','rejected','red_flushed') NOT NULL DEFAULT 'pending',
    tax_invoice_no VARCHAR(32) NULL COMMENT '税务发票号码, 开票时登记',
    pdf_blob MEDIUMBLOB NULL COMMENT '发票 PDF(已开票时写入)',
    pdf_filename VARCHAR(128) NULL,
    reject_reason VARCHAR(255) NULL COMMENT '驳回原因',
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '申请时间',
    issued_at TIMESTAMP NULL COMMENT '开票时间',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_buyer (buyer_id, status),
    INDEX idx_status (status, applied_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE invoice_orders (
    invoice_id BIGINT NOT NULL,
    order_id BIGINT NOT NULL,
    PRIMARY KEY (invoice_id, order_id),
    UNIQUE KEY uq_order (order_id) COMMENT '一个订单只能开一次票(驳回后由查询口径释放)'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
