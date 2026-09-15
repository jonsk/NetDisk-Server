-- +goose Up
-- ============================================================================
-- 00001_init.sql — 网盘系统初始 schema
-- 依据: 架构文档 V3.0 §6.4 元数据模型 + §4.1/4.2/4.3 + ADR-1(PG 18)
-- 要点:
--   * PG 18 原生 uuidv7() 作为主键默认值(11 章:时间有序 id)
--   * files.etag 为 STORED 生成列,值**不含双引号**(引号由序列化层 quoteETag 统一加,R-21)
--   * file_objects 四态 + 不变式 ref_count=0 ⟺ state<>'live'(R-06)
--   * 目录内判重统一 (space_id, parent_id, lower(name)) 唯一索引(6.4 P2-2)
--   * 每空间一个 BIGSERIAL 计数器行 → sync_feed.change_seq(6.4 T2-2 方案)
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ---------------------------------------------------------------- 用户与身份
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    username      varchar(64)  NOT NULL,
    email         varchar(255),
    display_name  varchar(128) NOT NULL DEFAULT '',
    avatar_url    text         NOT NULL DEFAULT '',
    role          text         NOT NULL DEFAULT 'user'
                  CHECK (role IN ('super_admin','dept_admin','user')),
    status        text         NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','disabled','pending')),
    -- 6.8 纪律 3:吊销/降权靠 token_version 自增比对实现"静默退出"
    token_version bigint       NOT NULL DEFAULT 1,
    ext_id_wecom     varchar(128),
    ext_id_dingtalk  varchar(128),
    created_at    timestamptz  NOT NULL DEFAULT now(),
    updated_at    timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_username_key ON users (lower(username));
CREATE UNIQUE INDEX users_email_key    ON users (lower(email)) WHERE email IS NOT NULL AND email <> '';
CREATE UNIQUE INDEX users_ext_wecom_key    ON users (ext_id_wecom)    WHERE ext_id_wecom IS NOT NULL;
CREATE UNIQUE INDEX users_ext_dingtalk_key ON users (ext_id_dingtalk) WHERE ext_id_dingtalk IS NOT NULL;

CREATE TABLE idp_providers (
    id               uuid PRIMARY KEY DEFAULT uuidv7(),
    name             varchar(64)  NOT NULL,
    kind             text         NOT NULL CHECK (kind IN ('wecom','dingtalk','oidc_generic','ldap')),
    corp_id          varchar(255) NOT NULL DEFAULT '',
    agent_id         varchar(255) NOT NULL DEFAULT '',
    secret_enc       text         NOT NULL DEFAULT '',   -- AES-256 密文,明文只在内存
    redirect_uri     text         NOT NULL DEFAULT '',
    auto_create_user boolean      NOT NULL DEFAULT true,
    sync_enabled     boolean      NOT NULL DEFAULT false,
    enabled          boolean      NOT NULL DEFAULT true,
    created_at       timestamptz  NOT NULL DEFAULT now(),
    updated_at       timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idp_providers_kind_corp_key ON idp_providers (kind, corp_id);

CREATE TABLE user_sso_bindings (
    id               uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id          uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    provider_id      uuid NOT NULL REFERENCES idp_providers(id) ON DELETE RESTRICT,
    provider_kind    text NOT NULL,
    external_subject varchar(255) NOT NULL,
    raw_user_info    jsonb NOT NULL DEFAULT '{}'::jsonb,
    bound_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX user_sso_bindings_subject_key ON user_sso_bindings (provider_id, external_subject);
CREATE INDEX user_sso_bindings_user_idx ON user_sso_bindings (user_id);
CREATE INDEX user_sso_bindings_raw_gin  ON user_sso_bindings USING gin (raw_user_info);

-- ---------------------------------------------------------------- 组织架构
CREATE TABLE departments (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    parent_id  uuid REFERENCES departments(id) ON DELETE RESTRICT,
    name       varchar(128) NOT NULL,
    sort_order int NOT NULL DEFAULT 0,
    source     text NOT NULL DEFAULT 'manual'
               CHECK (source IN ('manual','wecom','dingtalk','ldap','oidc-scim')),
    ext_id     varchar(128),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX departments_parent_idx ON departments (parent_id);
CREATE UNIQUE INDEX departments_ext_key ON departments (source, ext_id) WHERE ext_id IS NOT NULL;

-- 部门树用闭包表(6.4 T1-4:弃 ltree)
CREATE TABLE department_closure (
    ancestor_id   uuid NOT NULL REFERENCES departments(id) ON DELETE CASCADE,
    descendant_id uuid NOT NULL REFERENCES departments(id) ON DELETE CASCADE,
    depth         int  NOT NULL,
    PRIMARY KEY (ancestor_id, descendant_id)
);
CREATE INDEX department_closure_desc_idx ON department_closure (descendant_id);

CREATE TABLE user_departments (
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    department_id uuid NOT NULL REFERENCES departments(id) ON DELETE RESTRICT,
    is_primary    boolean NOT NULL DEFAULT false,
    PRIMARY KEY (user_id, department_id)
);

CREATE TABLE groups (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    name        varchar(128) NOT NULL,
    description text NOT NULL DEFAULT '',
    owner_id    uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id  uuid NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role      text NOT NULL DEFAULT 'member' CHECK (role IN ('owner','admin','member')),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);

-- ---------------------------------------------------------------- 空间(个人盘统一为 kind=personal)
CREATE TABLE spaces (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    kind        text NOT NULL CHECK (kind IN ('personal','team')),
    group_id    uuid REFERENCES groups(id) ON DELETE RESTRICT,   -- team 必填,personal 为 NULL
    owner_id    uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name        varchar(128) NOT NULL,
    quota_bytes bigint NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),  -- 0 = 不限制
    used_bytes  bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    frozen      boolean NOT NULL DEFAULT false,                       -- 管理员冻结(4.3)
    last_seq    bigint NOT NULL DEFAULT floor(extract(epoch from now()))::bigint,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT spaces_kind_group_chk CHECK (
        (kind = 'personal' AND group_id IS NULL) OR (kind = 'team' AND group_id IS NOT NULL)
    )
);
CREATE UNIQUE INDEX spaces_personal_owner_key ON spaces (owner_id) WHERE kind = 'personal';
CREATE UNIQUE INDEX spaces_team_group_key     ON spaces (group_id) WHERE kind = 'team';
CREATE INDEX spaces_owner_idx ON spaces (owner_id);

CREATE TABLE space_members (
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    permission text NOT NULL DEFAULT 'editor' CHECK (permission IN ('manager','editor','reader')),
    joined_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, user_id)
);

-- 每空间单调 change_seq(6.4 T2-2):用 spaces 行的 last_seq 作为计数器
-- 注意:goose 按分号切语句,函数体必须用 StatementBegin/End 包裹(否则 $ 引用被截断)
-- +goose StatementBegin
CREATE FUNCTION next_change_seq(p_space uuid) RETURNS bigint
LANGUAGE sql AS $fn$
    UPDATE spaces SET last_seq = last_seq + 1 WHERE id = p_space RETURNING last_seq;
$fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- 物理对象(四态,R-06)
CREATE TABLE file_objects (
    hash_sha256     varchar(64) PRIMARY KEY CHECK (hash_sha256 ~ '^[0-9a-f]{64}$'),
    size            bigint NOT NULL CHECK (size >= 0),
    storage_backend varchar(32) NOT NULL DEFAULT 'fs',
    object_key      text NOT NULL,
    ref_count       int NOT NULL DEFAULT 0 CHECK (ref_count >= 0),
    state           text NOT NULL DEFAULT 'live'
                    CHECK (state IN ('live','pending_delete','deleting','deleted')),
    delete_after    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- 不变式:ref_count = 0 ⟺ state <> 'live'
    CONSTRAINT file_objects_ref_state_chk CHECK ((ref_count = 0) = (state <> 'live'))
);
-- 到期待清理对象(软领取用);另配 state='deleting' 小索引供看护任务复位超时
CREATE INDEX file_objects_pending_idx ON file_objects (delete_after) WHERE state = 'pending_delete';
CREATE INDEX file_objects_deleting_idx ON file_objects (delete_after) WHERE state = 'deleting';

-- ---------------------------------------------------------------- 文件元数据
CREATE TABLE files (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    space_id     uuid NOT NULL REFERENCES spaces(id) ON DELETE RESTRICT,
    parent_id    uuid REFERENCES files(id) ON DELETE RESTRICT,
    owner_id     uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT, -- 创建者(审计主体),不参与判权
    name         varchar(255) NOT NULL,
    is_dir       boolean NOT NULL DEFAULT false,
    size         bigint NOT NULL DEFAULT 0 CHECK (size >= 0),
    mime_type    varchar(255) NOT NULL DEFAULT 'application/octet-stream',
    hash_sha256  varchar(64),
    version      bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    depth        int NOT NULL DEFAULT 0 CHECK (depth >= 0),
    -- R-21: STORED 生成列,**不含双引号**;目录行为 NULL
    etag         text GENERATED ALWAYS AS (
                     lpad(to_hex(version), 8, '0') || '-' || coalesce(substr(hash_sha256, 1, 8), '00000000')
                 ) STORED,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT files_dir_hash_chk CHECK ((is_dir AND hash_sha256 IS NULL) OR (NOT is_dir))
);
-- 每空间恰有一行根(parent_id IS NULL)
CREATE UNIQUE INDEX files_root_key ON files (space_id) WHERE parent_id IS NULL;
-- 目录内判重:大小写不敏感(6.7 规则 6 / 6.4 P2-2)
CREATE UNIQUE INDEX files_dir_name_key ON files (space_id, parent_id, lower(name)) WHERE parent_id IS NOT NULL;
CREATE INDEX files_list_idx  ON files (space_id, parent_id);
CREATE INDEX files_hash_idx  ON files (hash_sha256) WHERE hash_sha256 IS NOT NULL;
CREATE INDEX files_owner_idx ON files (owner_id);

-- ---------------------------------------------------------------- 变更流(6.4 / 6.9)
CREATE TABLE sync_feed (
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    change_seq bigint NOT NULL,
    file_id    uuid,
    kind       text NOT NULL CHECK (kind IN ('created','updated','moved','deleted','shared_in')),
    version    bigint,
    parent_id  uuid,
    name       varchar(255),
    old_parent_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, change_seq)
);
CREATE INDEX sync_feed_created_idx ON sync_feed (created_at);

-- 客户端游标上报(6.4 T2-4):权威值,Redis 仅加速
CREATE TABLE sync_cursors (
    client_id    text NOT NULL,
    space_id     uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    last_seq     bigint NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (client_id, space_id)
);
CREATE INDEX sync_cursors_seen_idx ON sync_cursors (last_seen_at);

-- ---------------------------------------------------------------- 上传任务与配额预留(4.3 / 6.10)
CREATE TABLE uploads (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    space_id       uuid NOT NULL REFERENCES spaces(id) ON DELETE RESTRICT,
    parent_id      uuid REFERENCES files(id) ON DELETE RESTRICT,
    name           varchar(255) NOT NULL,
    declared_size  bigint NOT NULL CHECK (declared_size >= 0),
    declared_hash  varchar(64),
    actual_size    bigint,
    actual_hash    varchar(64),
    -- reserved: 已预留额度;finalized: 已结算;released: 已释放;failed: 失败
    state          text NOT NULL DEFAULT 'reserved'
                   CHECK (state IN ('reserved','finalized','released','failed')),
    target_file_id uuid,
    ticket_hash    text NOT NULL,
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX uploads_user_state_idx ON uploads (user_id, state);
CREATE INDEX uploads_expire_idx     ON uploads (expires_at) WHERE state = 'reserved';

-- ---------------------------------------------------------------- 分享(6.3 / 1.1)
CREATE TABLE shares (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    token          varchar(64) NOT NULL,
    file_id        uuid NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    password_hash  text NOT NULL DEFAULT '',
    expires_at     timestamptz,
    max_downloads  int NOT NULL DEFAULT 0 CHECK (max_downloads >= 0),  -- 0 = 不限
    download_count int NOT NULL DEFAULT 0 CHECK (download_count >= 0),
    revoked        boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX shares_token_key ON shares (token);
CREATE INDEX shares_file_idx ON shares (file_id);

-- ---------------------------------------------------------------- 编辑锁(REST 锁,替代 WebDAV LOCK,6.2/6.3)
CREATE TABLE file_locks (
    file_id    uuid PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- 审计(4.4,按月分区)
CREATE TABLE audit_logs (
    id          uuid NOT NULL DEFAULT uuidv7(),
    created_at  timestamptz NOT NULL DEFAULT now(),
    user_id     uuid,
    space_id    uuid,
    action      varchar(64) NOT NULL,
    target_type varchar(32) NOT NULL DEFAULT '',
    target_id   text NOT NULL DEFAULT '',
    result_code varchar(64) NOT NULL DEFAULT '',
    ip          inet,
    user_agent  text NOT NULL DEFAULT '',
    request_id  varchar(64) NOT NULL DEFAULT '',
    bytes       bigint NOT NULL DEFAULT 0,
    duration_ms int NOT NULL DEFAULT 0,
    share_token varchar(64),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX audit_logs_user_time_idx ON audit_logs (user_id, created_at DESC);
CREATE INDEX audit_logs_action_idx    ON audit_logs (action, created_at DESC);
CREATE INDEX audit_logs_request_idx   ON audit_logs (request_id);
-- 覆盖当前月与下月,后续由定时任务滚动创建
-- +goose StatementBegin
DO $do$
DECLARE
    m date := date_trunc('month', now())::date;
BEGIN
    EXECUTE format('CREATE TABLE IF NOT EXISTS audit_logs_%s PARTITION OF audit_logs FOR VALUES FROM (%L) TO (%L)',
                   to_char(m, 'YYYYMM'), m, (m + interval '1 month')::date);
    EXECUTE format('CREATE TABLE IF NOT EXISTS audit_logs_%s PARTITION OF audit_logs FOR VALUES FROM (%L) TO (%L)',
                   to_char(m + interval '1 month', 'YYYYMM'), (m + interval '1 month')::date, (m + interval '2 month')::date);
END $do$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS audit_logs CASCADE;
DROP TABLE IF EXISTS file_locks CASCADE;
DROP TABLE IF EXISTS shares CASCADE;
DROP TABLE IF EXISTS uploads CASCADE;
DROP TABLE IF EXISTS sync_cursors CASCADE;
DROP TABLE IF EXISTS sync_feed CASCADE;
DROP TABLE IF EXISTS files CASCADE;
DROP TABLE IF EXISTS file_objects CASCADE;
DROP FUNCTION IF EXISTS next_change_seq(uuid);
DROP TABLE IF EXISTS space_members CASCADE;
DROP TABLE IF EXISTS spaces CASCADE;
DROP TABLE IF EXISTS group_members CASCADE;
DROP TABLE IF EXISTS groups CASCADE;
DROP TABLE IF EXISTS user_departments CASCADE;
DROP TABLE IF EXISTS department_closure CASCADE;
DROP TABLE IF EXISTS departments CASCADE;
DROP TABLE IF EXISTS user_sso_bindings CASCADE;
DROP TABLE IF EXISTS idp_providers CASCADE;
DROP TABLE IF EXISTS users CASCADE;
