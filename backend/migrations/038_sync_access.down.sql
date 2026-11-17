-- 038: 回退会使已有 sync_access 任务行的 kind 失效; 须先确认无此类行并人工处理。
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '038 downgrade requires removing sync_access task rows first; automatic enum shrink is disabled';
