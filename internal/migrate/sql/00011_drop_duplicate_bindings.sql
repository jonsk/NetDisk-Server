-- +goose Up
-- 订正:删除**重复的**绑定表,回到 00001 已有的 `user_sso_bindings` + `idp_providers`。
--
-- 背景(必须写下来,否则后人还会踩):
--   00001 早就定义了这两张表,而且正是 BE-S2-08 验收里说的那两个东西 ——
--     `idp_providers(id, name, kind, corp_id, agent_id, secret_enc, redirect_uri,
--                    auto_create_user, sync_enabled, enabled)`  + `(kind, corp_id)` 唯一
--     `user_sso_bindings(id, user_id, provider_id, provider_kind, external_subject,
--                        raw_user_info jsonb, bound_at)`      + GIN(raw_user_info)
--   我在实现 BE-S2-05/07 时**没有先看初始 schema**,另起了一张 `user_idp_bindings`,
--   于是同一个概念有了两套账 —— 这正是本项目反复强调要避免的东西
--   (9.15 口径单一):两套账不会立刻报错,而是在"某天发现两边人数不一样"时爆发,
--   而那时已经没人知道哪边是对的。
--
-- 为什么用新迁移而不是改旧文件:00008/00010 已经在环境里应用过(goose 记了版本),
-- 改一个已应用的迁移不会让任何已有库跟着变 —— 迁移是历史,订正要新开一个。
DROP TABLE IF EXISTS user_idp_bindings;

-- +goose Down
-- 不回滚:重复表已删除,回滚只会把"两套账"再造出来。
SELECT 1;
