-- 009_ticket_support.down.sql
-- 回滚工单售后; 工单与沟通记录将被删除, 生产执行前务必备份。

DROP TABLE IF EXISTS ticket_messages;
DROP TABLE IF EXISTS tickets;
