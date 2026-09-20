-- 留资表单补齐设计要求的字段 (docs/07 REQ-E-021, 线稿留资卡):
-- company_name 企业名称(融资租赁必填、其余可选), source 线索来源(转化追踪 REQ-E-042)。
ALTER TABLE leads
    ADD COLUMN company_name VARCHAR(128) NULL AFTER contact_email,
    ADD COLUMN source VARCHAR(32) NULL AFTER term;
