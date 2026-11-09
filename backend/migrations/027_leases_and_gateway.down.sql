-- Leases, access sessions, outages and partial refunds are business records; rollback needs an audited archival plan.
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '027 downgrade requires audited lease/session archival; automatic table removal is disabled';
