-- 直连映射访问模式(fleet.access_mode=direct): 买家公钥变更/解冻/重启后把租约容器的买家公钥与网络连接同步到节点。
ALTER TABLE node_tasks MODIFY COLUMN kind ENUM('probe','bootstrap_key','apply_firewall','provision','selftest','park','unpark',
    'kill_sessions','stop','purge','pull_model','deploy_model','stop_model','sync_access') NOT NULL;
