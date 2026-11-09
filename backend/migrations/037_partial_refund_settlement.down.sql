-- T-106: settlements 扣减/补偿列与 supplier_breach 退款行是资金记录, 回滚需经审计的归档方案。
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '037 downgrade requires audited refund/settlement archival; automatic column removal is disabled';
