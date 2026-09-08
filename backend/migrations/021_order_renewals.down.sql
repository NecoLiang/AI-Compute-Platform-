-- Application rollback must preserve paid renewal and delivery history.
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '021 downgrade requires audited renewal archival; automatic table removal is disabled';
