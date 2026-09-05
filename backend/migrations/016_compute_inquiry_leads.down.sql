-- Compute inquiries must be archived before removing this enum value.
ALTER TABLE leads MODIFY COLUMN type ENUM('equipment','construction','finance_lease') NOT NULL;
