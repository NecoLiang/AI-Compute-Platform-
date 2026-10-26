-- 易宝支付接入(平台商收付款方案): 支付 → 清算 → 分账 → 退款全链路状态落库。
-- payments.yeepay_tx_id 存易宝收款订单号(uniqueOrderNo); 金额一律为分。
ALTER TABLE payments
    ADD COLUMN cleared_at TIMESTAMP NULL COMMENT '易宝清算入账时间(可发起分账)' AFTER paid_at,
    ADD COLUMN refund_status ENUM('pending','processing','success','failed') NULL COMMENT 'NULL=无需退款' AFTER cleared_at,
    ADD COLUMN refund_request_id VARCHAR(64) NULL AFTER refund_status,
    ADD COLUMN refund_reason VARCHAR(256) NOT NULL DEFAULT '' AFTER refund_request_id,
    ADD COLUMN refunded_at TIMESTAMP NULL AFTER refund_reason,
    ADD UNIQUE KEY uk_refund_request (refund_request_id),
    ADD INDEX idx_refund_status (refund_status);

-- cancelled: 未分账即退款, 该笔应结不再发生; reversed: 已分账后随退款从供给方商编归还。
ALTER TABLE settlements
    MODIFY COLUMN status ENUM('pending','processing','success','failed','cancelled','reversed') DEFAULT 'pending',
    ADD COLUMN divide_request_id VARCHAR(64) NULL AFTER status,
    ADD COLUMN divide_detail_no VARCHAR(64) NULL COMMENT '易宝分账明细单号, 退款归还时回传' AFTER divide_request_id,
    ADD COLUMN ledger_no VARCHAR(32) NULL COMMENT '分账接收方易宝商编' AFTER divide_detail_no,
    ADD COLUMN fail_reason VARCHAR(256) NOT NULL DEFAULT '' AFTER ledger_no,
    ADD COLUMN updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP;

-- 供给方易宝商编: H5 自助入网或运营后台人工绑定。商编开通(COMPLETED)后才参与分账。
CREATE TABLE yeepay_merchants (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    request_no VARCHAR(64) NOT NULL,
    merchant_no VARCHAR(32) NULL,
    application_status VARCHAR(32) NOT NULL DEFAULT 'INIT' COMMENT '易宝入网状态, COMPLETED 为已开通',
    netin_url TEXT NULL,
    audit_opinion VARCHAR(512) NOT NULL DEFAULT '',
    bound_by BIGINT NULL COMMENT '人工绑定的运营 id',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_user (user_id),
    UNIQUE KEY uk_request (request_no),
    UNIQUE KEY uk_merchant (merchant_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
