CREATE TABLE order_renewals (
    order_id BIGINT NOT NULL PRIMARY KEY,
    parent_order_id BIGINT NOT NULL,
    request_id CHAR(36) NOT NULL,
    mode ENUM('extend','restart') NOT NULL,
    pricing_mode VARCHAR(20) NOT NULL,
    lease_end_at DATETIME NOT NULL,
    renewed_until DATETIME NULL,
    applied_at DATETIME NULL,
    previous_lease_start_at DATETIME NULL,
    previous_confirmed_at DATETIME NULL,
    confirmed_at DATETIME NULL,
    UNIQUE KEY uq_renewal_request (parent_order_id, request_id),
    CONSTRAINT fk_renewal_order FOREIGN KEY (order_id) REFERENCES orders(id),
    CONSTRAINT fk_renewal_parent FOREIGN KEY (parent_order_id) REFERENCES orders(id),
    CONSTRAINT chk_renewal_parent CHECK (order_id <> parent_order_id)
);
