-- +goose Up
-- IdP 身份与本地账号的绑定表(BE-S2-05 后半 / BE-S2-07)。
--
-- 为什么需要单独一张表而不是给 users 加两列:
--   ①一个用户**可能绑多个 IdP**(先企微后钉钉、或换公司入口),列式只能存一个;
--   ②解绑/换绑是**一行删除**,而不是"把两列置空"—— 后者会在 users 上留下
--     "半绑定"状态(有 external_id 却没 kind),而那种状态没有唯一索引能兜住;
--   ③唯一性约束天然是两维的:(kind, external_id) 唯一 = 一个外部身份只对应一个
--     本地账号(防"同一企微用户在库里变成两个账号");(user_id, kind) 唯一 =
--     一个本地账号在同一家 IdP 里只绑一个身份(防换绑后旧身份仍能登录)。
CREATE TABLE user_idp_bindings (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    kind        varchar(32) NOT NULL,          -- wecom / dingtalk / ...
    external_id varchar(191) NOT NULL,         -- 企微 userid / 钉钉 unionid
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- 绑定时的企业标识:换企业后旧绑定不该继续可用(5.4 的 corp 校验)
    corp_id     varchar(128) NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
-- 外部身份唯一:同一 (kind, external_id) 只能绑到一个本地账号
CREATE UNIQUE INDEX user_idp_bindings_external_key ON user_idp_bindings (kind, lower(external_id));
-- 本地账号在同一家 IdP 只绑一个身份
CREATE UNIQUE INDEX user_idp_bindings_user_kind_key ON user_idp_bindings (user_id, kind);
CREATE INDEX user_idp_bindings_user_idx ON user_idp_bindings (user_id);

-- +goose Down
DROP TABLE user_idp_bindings;
