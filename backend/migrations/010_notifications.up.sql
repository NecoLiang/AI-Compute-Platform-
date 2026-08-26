-- 010_notifications.up.sql
-- 消息中心: 买家通知(REQ-D-022 账单/消息区, 工单/发票/订单事件联动)。
--
-- 口径:
--   - 通知由业务事件同步产生(工单回复/状态、发票审核、订单交付/退款/取消),
--     暂无独立运营后台与异步投递队列, 不产生通知的事件即不存在。
--   - 类型: system=系统通知 order=订单动态 ticket=工单消息。
--   - 已读用 read_at 时间戳表达; 删除为软删(deleted_at), 保留审计痕迹。

CREATE TABLE notifications (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL COMMENT '接收用户',
    type ENUM('system','order','ticket') NOT NULL,
    title VARCHAR(128) NOT NULL,
    content VARCHAR(512) NOT NULL DEFAULT '',
    link VARCHAR(255) NOT NULL DEFAULT '' COMMENT '前端跳转链接, 如 /console/buyer/orders/xxx',
    read_at TIMESTAMP NULL COMMENT 'NULL=未读',
    deleted_at TIMESTAMP NULL COMMENT '软删',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_user (user_id, deleted_at, read_at, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
