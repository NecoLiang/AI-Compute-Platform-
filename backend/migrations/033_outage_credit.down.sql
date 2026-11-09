-- lease_outages/credit_scores 新列承载补偿与供给方责任金额等业务记录, 回滚需经审计的归档方案。
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '033 downgrade requires audited outage/credit archival; automatic column removal is disabled';
