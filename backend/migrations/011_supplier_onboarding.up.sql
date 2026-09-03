ALTER TABLE supplier_qualifications
    ADD COLUMN metadata_json TEXT NULL AFTER cert_url;
