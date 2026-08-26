-- 010_notifications.down.sql
-- 回滚消息中心; 通知数据将被删除, 生产执行前务必备份。

DROP TABLE IF EXISTS notifications;
