-- +goose Up
-- ============================================================================
-- 00015_user_phone_and_idp_enums.sql
--   1. users 加 phone(可空,部分唯一索引)
--   2. 扩展 idp_providers.kind 的 CHECK(加 keycloak/casdoor/zhuyun)
--   3. 扩展 departments.source 的 CHECK(加 keycloak/casdoor/zhuyun)
--
-- ⚠ 两列 CHECK 是**不同子集**(非 9 值全集):
--   - idp_providers.kind: 不含 manual(IdP 不是"手工"的)、不含 oidc-scim(SCIM 同步落到部门层)
--   - departments.source: 含 manual(手工建部门)、不含 oidc_generic(部门来源用 oidc-scim)
--
-- 不新增 users.external_id / users.identity_provider:
--   用户级外部身份已由 user_sso_bindings(provider_id, external_subject) 关联表承载(一对多,
--   原生支持多源联邦),新增扁平列属重复且退化单源。
-- ============================================================================

-- ---------------------------------------------------------------- users.phone
ALTER TABLE users ADD COLUMN phone varchar(20) NULL;

-- 部分唯一索引:空值不参与唯一(现有无手机用户不冲突)。
CREATE UNIQUE INDEX uq_users_phone ON users(phone) WHERE phone IS NOT NULL;

-- -------------------------------------------------- idp_providers.kind CHECK
-- 原 CHECK(00001_init.sql:41): ('wecom','dingtalk','oidc_generic','ldap')
-- 新 CHECK: 加 keycloak/casdoor/zhuyun(不含 manual/oidc-scim)
ALTER TABLE idp_providers DROP CONSTRAINT IF EXISTS idp_providers_kind_check;
ALTER TABLE idp_providers ADD CONSTRAINT idp_providers_kind_check
    CHECK (kind IN ('wecom','dingtalk','oidc_generic','ldap','keycloak','casdoor','zhuyun'));

-- -------------------------------------------------- departments.source CHECK
-- 原 CHECK(00001_init.sql:74): ('manual','wecom','dingtalk','ldap','oidc-scim')
-- 新 CHECK: 加 keycloak/casdoor/zhuyun(不含 oidc_generic)
ALTER TABLE departments DROP CONSTRAINT IF EXISTS departments_source_check;
ALTER TABLE departments ADD CONSTRAINT departments_source_check
    CHECK (source IN ('manual','wecom','dingtalk','ldap','oidc-scim','keycloak','casdoor','zhuyun'));

-- +goose Down
-- 回滚:先删 phone 列(连带索引),再把两处 CHECK 收回原值集。

DROP INDEX IF EXISTS uq_users_phone;
ALTER TABLE users DROP COLUMN IF EXISTS phone;

ALTER TABLE departments DROP CONSTRAINT IF EXISTS departments_source_check;
ALTER TABLE departments ADD CONSTRAINT departments_source_check
    CHECK (source IN ('manual','wecom','dingtalk','ldap','oidc-scim'));

ALTER TABLE idp_providers DROP CONSTRAINT IF EXISTS idp_providers_kind_check;
ALTER TABLE idp_providers ADD CONSTRAINT idp_providers_kind_check
    CHECK (kind IN ('wecom','dingtalk','oidc_generic','ldap'));
