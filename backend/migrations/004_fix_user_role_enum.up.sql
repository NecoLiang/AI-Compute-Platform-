-- 004_fix_user_role_enum.up.sql
-- 修复早期 001 的角色 ENUM 缺失；管理员账号必须通过受控运维流程显式创建。
ALTER TABLE user_roles
    MODIFY COLUMN role ENUM('buyer','supplier','vendor','funder','operator','admin') NOT NULL;
