ALTER TABLE blockchain_attestations
    DROP INDEX idx_chain_status,
    DROP COLUMN last_error,
    DROP COLUMN attempts;
