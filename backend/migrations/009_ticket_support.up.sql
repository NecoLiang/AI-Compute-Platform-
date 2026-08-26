-- 009_ticket_support.up.sql
-- 工单售后: 买家对订单发起工单(REQ-A-044 / REQ-D-020), 运营介入处理(REQ-D-035)。
--
-- 口径:
--   - 工单必须关联买家本人订单; 工单号 WO-YYYYMMDD-NNN 按日递增。
--   - 状态机: pending(待处理) -> processing(处理中, 运营受理) -> resolved(已完结, 运营完成)
--     -> closed(已关闭, 买家或运营关闭); closed 为终态不可恢复。
--   - 沟通记录独立成表, 买家/运营双侧追加, 不允许修改历史消息。

CREATE TABLE tickets (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    ticket_no VARCHAR(32) NOT NULL UNIQUE COMMENT '工单号 WO-YYYYMMDD-NNN',
    buyer_id BIGINT NOT NULL,
    order_no VARCHAR(32) NOT NULL COMMENT '关联订单号',
    type VARCHAR(24) NOT NULL COMMENT 'refund_dispute/resource_fault/unavailable/appeal/other',
    title VARCHAR(128) NOT NULL,
    content TEXT NOT NULL COMMENT '问题描述',
    status ENUM('pending','processing','resolved','closed') NOT NULL DEFAULT 'pending',
    resolved_at TIMESTAMP NULL,
    closed_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_buyer (buyer_id, status),
    INDEX idx_status (status, created_at),
    INDEX idx_order (order_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ticket_messages (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    ticket_id BIGINT NOT NULL,
    sender_type ENUM('buyer','operator') NOT NULL,
    sender_id BIGINT NOT NULL,
    content TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_ticket (ticket_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
