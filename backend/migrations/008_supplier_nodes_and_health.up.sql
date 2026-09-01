-- 供应方节点探活与容量调度 (docs/23):
-- supplier_nodes  节点注册表, 心跳密钥仅存 SHA-256 哈希
-- node_heartbeats 心跳流水(近期), 供在线率统计与容量趋势
-- products.health 节点健康聚合到商品维度: 全离线的商品拦截下单

ALTER TABLE products
    ADD COLUMN health ENUM('unknown','healthy','degraded','offline') NOT NULL DEFAULT 'unknown' AFTER status;

CREATE TABLE supplier_nodes (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    supplier_id BIGINT NOT NULL,
    product_id BIGINT NOT NULL,
    node_name VARCHAR(64) NOT NULL,
    node_key_hash CHAR(64) NOT NULL,
    status ENUM('online','degraded','offline') NOT NULL DEFAULT 'offline',
    total_cards INT NOT NULL DEFAULT 0,
    available_cards INT NOT NULL DEFAULT 0,
    gpu_util_pct TINYINT UNSIGNED NULL,
    vram_util_pct TINYINT UNSIGNED NULL,
    last_heartbeat_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_product_node (product_id, node_name),
    INDEX idx_supplier (supplier_id),
    INDEX idx_status_heartbeat (status, last_heartbeat_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE node_heartbeats (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    node_id BIGINT NOT NULL,
    available_cards INT NOT NULL,
    gpu_util_pct TINYINT UNSIGNED NULL,
    vram_util_pct TINYINT UNSIGNED NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_node_time (node_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
