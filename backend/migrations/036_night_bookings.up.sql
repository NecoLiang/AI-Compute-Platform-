-- 036 包夜预订(docs/26 §26.6.3, docs/24 T-094/T-095)。
--   night_bookings: 卡位 × 晚 防重(唯一键 uk_slot_night), 一单 k 卡 × n 晚 = k×n 行, 所有晚同一组卡;
--   lease_nights:   每租约每晚一行的开停机进度(开通/失败/结束), 驱动每晚开停机任务与 night_unpark_failed 停机认定。
-- night_date = 窗口开始所在日期(Asia/Shanghai)。

CREATE TABLE night_bookings (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    slot_id BIGINT NOT NULL,
    night_date DATE NOT NULL COMMENT '窗口开始所在日期',
    lease_id BIGINT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_slot_night (slot_id, night_date),
    INDEX idx_lease (lease_id),
    INDEX idx_night (night_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE lease_nights (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    lease_id BIGINT NOT NULL,
    night_date DATE NOT NULL COMMENT '窗口开始所在日期',
    state ENUM('booked','active','failed','ended') NOT NULL DEFAULT 'booked'
        COMMENT 'booked 待开通 / active 本晚已开通 / failed 本晚判定开通失败 / ended 窗口已结束',
    attempts INT NOT NULL DEFAULT 0 COMMENT '本晚开通尝试次数',
    last_error VARCHAR(512) NULL,
    occupied TINYINT NOT NULL DEFAULT 0 COMMENT '最近一次失败是否因卡被宿主机进程占用',
    unparked_at TIMESTAMP NULL COMMENT '本晚开通(自检通过)时刻',
    failed_at TIMESTAMP NULL,
    ended_at TIMESTAMP NULL,
    outage_id BIGINT NULL COMMENT '本晚记入的 lease_outages.id(night_unpark_failed / provision_delay)',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_lease_night (lease_id, night_date),
    INDEX idx_state_night (state, night_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
