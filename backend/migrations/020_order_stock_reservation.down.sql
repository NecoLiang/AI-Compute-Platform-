-- Automatic downgrade would erase inventory evidence and revive unsafe release logic.
-- Keep this column on application rollback; see docs/api/trade-controls-release.md.
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '020 downgrade requires audited stock reconciliation; automatic column removal is disabled';
