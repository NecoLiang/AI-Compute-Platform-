ALTER TABLE supplier_qualifications
    ADD COLUMN license_file_name VARCHAR(255) NULL AFTER metadata_json,
    ADD COLUMN license_content_type VARCHAR(128) NULL AFTER license_file_name,
    ADD COLUMN license_blob MEDIUMBLOB NULL AFTER license_content_type;
