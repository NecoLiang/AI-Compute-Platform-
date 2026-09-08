-- NULL means legacy occupancy is unknown and must be reconciled before release.
-- Do not infer occupancy from an order number: fixtures and renewals did not reserve stock.
ALTER TABLE orders ADD COLUMN stock_reserved INT NULL DEFAULT NULL
    COMMENT 'Outstanding stock reserved by this order; NULL requires reconciliation';
ALTER TABLE orders ADD CONSTRAINT chk_order_stock_reserved
    CHECK (stock_reserved IS NULL OR (stock_reserved >= 0 AND stock_reserved <= quantity));
UPDATE orders SET stock_reserved=0 WHERE status IN ('cancelled','refunded','completed');
