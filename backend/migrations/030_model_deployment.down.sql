-- Deployment records, real-machine verification records and health samples are business data.
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '030 downgrade requires audited archival of model catalog, verifications and deployments; automatic removal is disabled';
