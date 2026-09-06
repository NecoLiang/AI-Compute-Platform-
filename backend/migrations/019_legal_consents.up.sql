CREATE TABLE IF NOT EXISTS legal_consents (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    document_key VARCHAR(32) NOT NULL,
    document_version VARCHAR(32) NOT NULL,
    action VARCHAR(32) NOT NULL,
    reference_id VARCHAR(64) NOT NULL,
    accepted_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idx_legal_consents_user (user_id, id),
    CONSTRAINT fk_legal_consents_user FOREIGN KEY (user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Existing accounts and transactions have no provable versioned acceptance. Do not backfill consent.
