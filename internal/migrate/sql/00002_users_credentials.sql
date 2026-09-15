-- +goose Up
-- ============================================================================
-- 00002_users_credentials.sql
-- 补齐本地账密登录所需的凭证列(6.3「用户名密码登录」)。
-- 之前 00001 只建了用户模型,没有落密码哈希列 —— 这里补上,而不是回改 00001,
-- 以保持"迁移一旦发布不再修改"的纪律。
-- ============================================================================

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS password_hash text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS password_updated_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_login_at timestamptz,
    ADD COLUMN IF NOT EXISTS failed_login_count int NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS locked_until timestamptz;

COMMENT ON COLUMN users.password_hash IS 'bcrypt 哈希;空串表示仅允许 SSO 登录';
COMMENT ON COLUMN users.failed_login_count IS '连续失败次数,达阈值后短时锁定';
COMMENT ON COLUMN users.locked_until IS '锁定到期时间;NULL 表示未锁定';

-- 便于运维按"最近登录"做账号治理
CREATE INDEX IF NOT EXISTS users_last_login_idx ON users (last_login_at DESC NULLS LAST);

-- refresh token 的**权威存储**(6.8 纪律 3:Redis 只做缓存,DB 为账本)。
-- 这样 Redis 重启不会导致全员强制登出,与 10.7「Redis 重启代价=一次刷新」的承诺一致。
CREATE TABLE IF NOT EXISTS refresh_tokens (
    jti        varchar(64) PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    audience   text NOT NULL,
    token_version bigint NOT NULL,
    issued_at  timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX IF NOT EXISTS refresh_tokens_expire_idx ON refresh_tokens (expires_at) WHERE revoked_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS refresh_tokens;
DROP INDEX IF EXISTS users_last_login_idx;
ALTER TABLE users
    DROP COLUMN IF EXISTS password_hash,
    DROP COLUMN IF EXISTS password_updated_at,
    DROP COLUMN IF EXISTS last_login_at,
    DROP COLUMN IF EXISTS failed_login_count,
    DROP COLUMN IF EXISTS locked_until;
