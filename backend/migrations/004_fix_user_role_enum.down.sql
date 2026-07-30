-- 004_fix_user_role_enum.down.sql
-- 回滚到 001 原始的 ENUM 定义。
-- 注意: 回滚前必须先清理 operator/admin 角色行, 否则 MODIFY 会因数据截断失败。
DELETE FROM user_roles WHERE role IN ('operator', 'admin');

ALTER TABLE user_roles
    MODIFY COLUMN role ENUM('buyer','supplier','vendor','funder') NOT NULL;
