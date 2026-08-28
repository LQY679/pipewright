-- 0051: credentials 增加 OAuth access_token 过期时间(RFC3339 UTC)。
-- 用于「过期/临期才刷新」:NULL/空 = 未知(存量凭据首次使用会刷新一次并补记)。
ALTER TABLE credentials ADD COLUMN expires_at VARCHAR(64) NULL;
