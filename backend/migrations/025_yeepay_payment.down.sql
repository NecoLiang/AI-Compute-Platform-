DROP TABLE IF EXISTS yeepay_merchants;
UPDATE settlements SET status='failed' WHERE status IN ('cancelled','reversed');
ALTER TABLE settlements
    DROP COLUMN updated_at,
    DROP COLUMN fail_reason,
    DROP COLUMN ledger_no,
    DROP COLUMN divide_detail_no,
    DROP COLUMN divide_request_id,
    MODIFY COLUMN status ENUM('pending','processing','success','failed') DEFAULT 'pending';
ALTER TABLE payments
    DROP INDEX idx_refund_status,
    DROP INDEX uk_refund_request,
    DROP COLUMN refunded_at,
    DROP COLUMN refund_reason,
    DROP COLUMN refund_request_id,
    DROP COLUMN refund_status,
    DROP COLUMN cleared_at;
