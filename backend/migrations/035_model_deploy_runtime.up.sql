-- T-098/T-099: 一键部署流水线与健康检测运行期字段(docs/27 §27.4/§27.5, "S5 实现口径")。
-- 030 的表结构不变, 只追加列与事件表。

ALTER TABLE model_deployments
    ADD COLUMN state_changed_at TIMESTAMP NULL COMMENT '最近一次 state 迁移时间; 启动超时/退避均以此为起点' AFTER state,
    ADD COLUMN next_restart_at TIMESTAMP NULL COMMENT '已排定的自动重启时间(退避 1/5/15 分钟); 重启已下发后清空' AFTER last_restart_at,
    ADD COLUMN engine_cpus INT NULL COMMENT '引擎容器 --cpus(租约配额拆分后)' AFTER container_name,
    ADD COLUMN engine_memory_gb INT NULL COMMENT '引擎容器 --memory(GB)' AFTER engine_cpus,
    ADD COLUMN lease_cpus INT NULL COMMENT '租约容器完整配额 --cpus(部署期间缩减为 lease_cpus-engine_cpus, 停止时据此归还)' AFTER engine_memory_gb,
    ADD COLUMN lease_memory_gb INT NULL COMMENT '租约容器完整配额 --memory(GB)(部署期间缩减为 lease_memory_gb-engine_memory_gb, 停止时据此归还)' AFTER lease_cpus,
    ADD COLUMN monitor_state JSON NULL COMMENT '健康判定跨轮状态: 连续失败计数/degraded 起始时间/上轮计数器与直方图' AFTER baseline_canary_ms,
    ADD COLUMN stop_reason VARCHAR(32) NULL COMMENT 'buyer/lease_expired/lease_frozen/lease_purged/nightly' AFTER fail_reason,
    ADD INDEX idx_order (order_id),
    ADD INDEX idx_lease (lease_id),
    ADD INDEX idx_node_model (node_id, model_id);

ALTER TABLE node_model_cache
    ADD COLUMN pull_container VARCHAR(64) NULL COMMENT '拉取容器名(downloading 期间)' AFTER last_error,
    ADD COLUMN pull_started_at TIMESTAMP NULL COMMENT '拉取容器启动时间; 超过上限判失败' AFTER pull_container;

-- 部署事件: 重启记录(买家页面展示)、自动重启滚动 1 小时计数、degraded 通知 30 分钟去重。
CREATE TABLE deployment_events (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    deployment_id BIGINT NOT NULL,
    kind ENUM('auto_restart','manual_restart','resume','stop','fail','running','degraded_notice','unhealthy_notice') NOT NULL,
    reason VARCHAR(64) NULL,
    detail VARCHAR(512) NULL,
    created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    INDEX idx_dep_kind_time (deployment_id, kind, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
