ALTER TABLE enterprises
    ADD COLUMN legal_person_id_card VARCHAR(32) NOT NULL DEFAULT '' AFTER legal_person,
    ADD COLUMN bank_name VARCHAR(128) NOT NULL DEFAULT '' AFTER legal_person_id_card,
    ADD COLUMN bank_account_name VARCHAR(128) NOT NULL DEFAULT '' AFTER bank_name,
    ADD COLUMN bank_account_number VARCHAR(64) NOT NULL DEFAULT '' AFTER bank_account_name,
    ADD COLUMN license_file_name VARCHAR(255) NOT NULL DEFAULT '' AFTER bank_account_number,
    ADD COLUMN license_content_type VARCHAR(128) NOT NULL DEFAULT '' AFTER license_file_name,
    ADD COLUMN license_blob MEDIUMBLOB NULL AFTER license_content_type;
