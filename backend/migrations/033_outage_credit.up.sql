-- T-105 停机认定/补偿 + 供给方信用分自动停调度 (docs/25 §25.7.6, docs/24 D-9/D-10/Q-47)

-- lease_outages: 结束后的有效计时(扣除与其他段的重叠)、是否计入(< fleet.outage.min_minutes 不计)、责任方承担金额
ALTER TABLE lease_outages
    ADD COLUMN node_id BIGINT NULL COMMENT '停机时租约所在节点, 按节点 30 天累计用' AFTER order_id,
    ADD COLUMN detail VARCHAR(255) NULL COMMENT '认定依据(探活/对账/告警原文摘要)',
    ADD COLUMN seconds INT NULL COMMENT '结束后计算: 扣除与同租约其他段重叠后的有效秒数',
    ADD COLUMN counted TINYINT NULL COMMENT '结束后计算: 1=计入补偿与信用, 0=短于 min_minutes 不计',
    ADD COLUMN liability_amount BIGINT NULL COMMENT '责任方(liable)承担金额(分) = floor(所属订单实付 × seconds ÷ 该订单租期秒数)',
    ADD INDEX idx_node_end (node_id, ended_at),
    ADD INDEX idx_end (ended_at);

-- leases: 买家已选择退费但退费通道(T-106)未就绪时记录意向, 补偿仍按顺延执行
ALTER TABLE leases
    ADD COLUMN outage_refund_requested_at TIMESTAMP NULL COMMENT '买家请求退费补偿的时间(达 Q-47 门槛), T-106 读取' AFTER outage_remedy;

-- credit_scores: T-023 信用分落地(原表无写入方)
ALTER TABLE credit_scores
    ADD COLUMN score INT NOT NULL DEFAULT 100 COMMENT '0-100, 低于 fleet.credit.stop_below 自动停调度',
    ADD COLUMN outage_minutes_30d INT NOT NULL DEFAULT 0 COMMENT '近 30 天供给方责任且计入的停机分钟',
    ADD COLUMN outage_count_30d INT NOT NULL DEFAULT 0,
    ADD COLUMN probe_failures_30d INT NOT NULL DEFAULT 0 COMMENT '近 30 天已验证节点完整探活失败次数(审计 node.verify_failed)',
    ADD COLUMN defaults_30d INT NOT NULL DEFAULT 0 COMMENT '近 30 天开通失败(租约 failed)次数',
    ADD COLUMN dispatch_stopped TINYINT NOT NULL DEFAULT 0 COMMENT '生效结果: 1=该供给方节点不接新托管租约',
    ADD COLUMN override_mode ENUM('auto','allow','stop') NOT NULL DEFAULT 'auto' COMMENT '运营覆盖: auto=按分数, allow=强制放行, stop=强制停调度',
    ADD COLUMN override_by BIGINT NULL,
    ADD COLUMN override_reason VARCHAR(255) NULL,
    ADD COLUMN override_at TIMESTAMP NULL,
    ADD COLUMN computed_at TIMESTAMP NULL;

-- supplier_nodes: 平台侧调度暂停(与供给方自己的 schedulable 开关分离, 探活/信用重算只写本列)
ALTER TABLE supplier_nodes
    ADD COLUMN dispatch_hold TINYINT NOT NULL DEFAULT 0 COMMENT '1=平台暂停向该节点分配新租约(信用分不足或近30天停机超限)',
    ADD COLUMN dispatch_hold_reason VARCHAR(255) NULL,
    ADD COLUMN dispatch_hold_at TIMESTAMP NULL;
