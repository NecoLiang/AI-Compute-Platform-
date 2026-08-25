-- 008_invoice_management.down.sql
-- 回滚发票管理。顺序与 up 相反; 发票数据将被删除, 生产执行前务必备份。

DROP TABLE IF EXISTS invoice_orders;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS invoice_titles;
