-- OAuth refresh_token (加密存储) 供 access_token 过期时静默续期。
-- 仅 OAuth 兑换时写入;手动创建的凭据保持 NULL。
ALTER TABLE credentials ADD COLUMN refresh_token_ciphertext VARBINARY(4096) NULL;
