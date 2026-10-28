-- 031 只含可从节点重新采集的探活遥测(逐卡读数、拓扑、Xid 扫描覆盖与事件明细、缓存目录容量), 不含订单/租约/计费等业务数据;
-- 停机认定与补偿(T-105)以 leases.resource_alert 与 lease_outages 为准, 不读取这里的明细。故降级直接删除, 无需归档。
DROP TABLE IF EXISTS gpu_xid_events;

ALTER TABLE supplier_nodes
    DROP COLUMN cache_note,
    DROP COLUMN cache_observed_at,
    DROP COLUMN cache_total_bytes,
    DROP COLUMN cache_free_bytes,
    DROP COLUMN xid_scan_note,
    DROP COLUMN xid_covered_from,
    DROP COLUMN xid_scan_ok_at;

ALTER TABLE gpu_slots
    DROP COLUMN topo_at,
    DROP COLUMN topo_links,
    DROP COLUMN telemetry_at,
    DROP COLUMN ecc_corrected,
    DROP COLUMN ecc_uncorrected,
    DROP COLUMN throttle_reasons,
    DROP COLUMN power_w,
    DROP COLUMN temp_c,
    DROP COLUMN vram_total_mb,
    DROP COLUMN vram_used_mb;
