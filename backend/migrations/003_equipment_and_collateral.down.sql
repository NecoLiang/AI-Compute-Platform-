-- 003_equipment_and_collateral.down.sql
-- 回滚 C-08 设备商品/询价 与 C-07 中登网动产融资登记

DROP TABLE IF EXISTS collateral_registrations;
DROP TABLE IF EXISTS equipment_inquiries;
DROP TABLE IF EXISTS equipment_products;
