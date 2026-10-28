-- 031 探活遥测(docs/27 §27.3 一键部署可行性 F3/F4/F5/F6 的数据源, docs/25 §25.4.4 周期探活):
--   gpu_slots      逐卡遥测(轻探活 60s 刷新)与两两拓扑(完整探活 12h 刷新);
--   supplier_nodes Xid 扫描覆盖区间(每次探活刷新)与模型缓存目录容量(完整探活刷新);
--   gpu_xid_events 内核日志里的 Xid 事件明细(按节点+原始行去重)。
-- 全部为可从节点重新采集的遥测, 只加表加列; 读取方按采集时间判新鲜度, 过期即视为"未知"。
ALTER TABLE gpu_slots
    ADD COLUMN vram_used_mb INT NULL COMMENT '显存已用 MiB(nvidia-smi memory.used); NULL=未采集或 [N/A]',
    ADD COLUMN vram_total_mb INT NULL COMMENT '同一次采样的显存总量 MiB(memory.total)',
    ADD COLUMN temp_c SMALLINT NULL COMMENT '温度 ℃',
    ADD COLUMN power_w DECIMAL(7,2) NULL COMMENT '功耗 W; 不支持时 NULL',
    ADD COLUMN throttle_reasons BIGINT UNSIGNED NULL COMMENT 'clocks_throttle_reasons.active 位图',
    ADD COLUMN ecc_uncorrected BIGINT NULL COMMENT 'ecc.errors.uncorrected.volatile.total; 不支持 ECC 时 NULL',
    ADD COLUMN ecc_corrected BIGINT NULL COMMENT 'ecc.errors.corrected.volatile.total',
    ADD COLUMN telemetry_at DATETIME NULL COMMENT '以上遥测的采集时间(平台时钟)',
    ADD COLUMN topo_links JSON NULL COMMENT '本卡到同节点各卡的连接, 按 gpu_index 排列, 如 ["X","NV18"]; 来自 nvidia-smi topo -m',
    ADD COLUMN topo_at DATETIME NULL COMMENT 'topo_links 的采集时间';

ALTER TABLE supplier_nodes
    ADD COLUMN xid_scan_ok_at DATETIME NULL COMMENT '最近一次成功(可读内核日志)的 Xid 扫描时间',
    ADD COLUMN xid_covered_from DATETIME NULL COMMENT '自该时刻起至 xid_scan_ok_at 的内核日志已被连续扫描; NULL=覆盖中断',
    ADD COLUMN xid_scan_note VARCHAR(255) NULL COMMENT '最近一次 Xid 扫描不可用的原因',
    ADD COLUMN cache_free_bytes BIGINT NULL COMMENT '模型缓存目录所在文件系统可用字节(df -B1)',
    ADD COLUMN cache_total_bytes BIGINT NULL COMMENT '模型缓存目录所在文件系统总字节',
    ADD COLUMN cache_observed_at DATETIME NULL COMMENT '缓存目录容量采集时间; 目录不存在/读取失败时只更新 cache_note',
    ADD COLUMN cache_note VARCHAR(255) NULL COMMENT '最近一次缓存目录容量采集失败原因';

CREATE TABLE gpu_xid_events (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    node_id BIGINT NOT NULL,
    pci_bus_id VARCHAR(32) NOT NULL COMMENT '归一化 PCI 地址 dddd:bb:dd, 与 gpu_slots.pci_bus_id 归一化后对应',
    xid INT NOT NULL,
    occurred_at DATETIME NOT NULL COMMENT '事件时间(已按节点与平台时钟差换算到平台时钟)',
    seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '平台首次读到该事件的时间',
    source ENUM('journal','dmesg') NOT NULL,
    message VARCHAR(512) NOT NULL COMMENT '原始日志行(截断)',
    event_key CHAR(64) NOT NULL COMMENT 'sha256(原始行), 同一行在相邻扫描窗口重复读到时去重',
    UNIQUE KEY uk_node_event (node_id, event_key),
    INDEX idx_node_bus_time (node_id, pci_bus_id, occurred_at),
    INDEX idx_seen (seen_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
