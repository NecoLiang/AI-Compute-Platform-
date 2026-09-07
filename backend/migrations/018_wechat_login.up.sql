CREATE TABLE IF NOT EXISTS user_wechat_identities (
    app_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    openid VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    unionid VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
    user_id BIGINT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (app_id, openid),
    UNIQUE KEY uk_user_wechat_app (user_id, app_id),
    CONSTRAINT fk_wechat_user FOREIGN KEY (user_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
