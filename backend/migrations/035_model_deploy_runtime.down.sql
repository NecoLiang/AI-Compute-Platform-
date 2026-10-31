-- deployment_events holds buyer-visible restart history and notification records (business data).
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '035 downgrade requires audited archival of deployment events; automatic removal is disabled';
