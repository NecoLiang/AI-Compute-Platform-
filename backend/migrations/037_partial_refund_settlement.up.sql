-- T-106 托管订单租期末一次分账 + 停机补偿部分退款 (docs/25 §25.7.6, docs/24 D-10)

-- payment_refunds: 新增供给方违约整单退款(未使用部分), 运营发起, 一条租约至多一次(source_id = leases.id)
ALTER TABLE payment_refunds
    MODIFY COLUMN kind ENUM('outage_compensation','supplier_breach') NOT NULL
        COMMENT 'outage_compensation: source_id=lease_outages.id; supplier_breach: source_id=leases.id; 全额退款继续走 payments.refund_status 原路径',
    ADD COLUMN reason VARCHAR(255) NULL COMMENT '退款原因(随退款请求送易宝的描述)' AFTER source_id,
    ADD INDEX idx_status (status, updated_at);

-- settlements: 分账前按已成功的部分退款重算应结, 记录扣减与平台另行补偿(不变量: 部分退款合计 + 两行 amount = 实付)
ALTER TABLE settlements
    ADD COLUMN refund_deduction BIGINT NOT NULL DEFAULT 0
        COMMENT '已成功部分退款从本行扣减的金额(分); 收款时锁定的应结 = amount + refund_deduction',
    ADD COLUMN platform_compensation BIGINT NOT NULL DEFAULT 0
        COMMENT '仅供给方行: 平台责任退款超出平台费、从供给方应结扣下的部分, 由平台另行补偿给供给方(记账, 不走二清)';
