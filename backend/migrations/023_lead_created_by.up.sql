-- 留资接口收进登录组后记录提交账号: 反刷追责 + 按用户限流的依据。
ALTER TABLE leads ADD COLUMN created_by BIGINT NULL AFTER source;
