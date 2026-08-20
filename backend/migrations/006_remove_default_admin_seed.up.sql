-- 006_remove_default_admin_seed.up.sql
-- 只删除早期迁移写入的固定 admin/admin123 账号；密码已变更的账号不会被误删。
DELETE ur
FROM user_roles ur
JOIN users u ON u.id = ur.user_id
WHERE u.phone = 'admin'
  AND u.password_hash = '$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy';

DELETE FROM users
WHERE phone = 'admin'
  AND password_hash = '$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy';
