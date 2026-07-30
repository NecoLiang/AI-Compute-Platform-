-- 004_fix_user_role_enum.up.sql
-- 修复 001 的缺陷: user_roles.role 的 ENUM 缺少 'operator' 与 'admin',
-- 导致 001 末尾的种子语句
--     INSERT INTO user_roles (user_id, role) VALUES (1,'buyer'),(1,'operator'),(1,'admin');
-- 整条中止 (ERROR 1265 Data truncated)，user_roles 表为空。
-- 后果: 种子管理员没有任何角色, main.go 中 mw.RBAC("operator","admin") 保护的
-- 全部运营后台路由在全新部署上不可达。
--
-- 001 中的 ENUM 定义已同步修正(供全新安装使用); 本迁移用于已经执行过 001 的库,
-- 可安全重复执行。
ALTER TABLE user_roles
    MODIFY COLUMN role ENUM('buyer','supplier','vendor','funder','operator','admin') NOT NULL;

-- 补种子管理员角色。已存在则忽略(uk_user_role 唯一键保证幂等)。
INSERT IGNORE INTO user_roles (user_id, role) VALUES (1, 'buyer'), (1, 'operator'), (1, 'admin');
