-- +goose Up
-- 【本条已被 00011 订正为"仅说明"】:它原本想新建 `idp_providers`,但 **00001 早就
-- 定义了这张表**(且正是 BE-S2-08 验收要用的那张:含 `secret_enc` 密文列、`(kind, corp_id)`
-- 唯一、`auto_create_user`/`sync_enabled` 开关),另有 `user_sso_bindings.raw_user_info jsonb`
-- 承载"原始用户信息"。
--
-- 我最初实现 IdP 时没有先读 00001,于是:
--   - 00008 另起了一张重复的绑定表 `user_idp_bindings`;
--   - 00010 又写了一遍 `idp_providers`(IF NOT EXISTS 让它静默变成空操作),
--     并把 `raw_user_info` 加到了那张**即将被删掉的**重复表上。
-- 迁移 **00011** 已删除重复表,代码也改回既有表。
--
-- 本文件保留为**空操作**而不是删掉:goose 按版本号记账,删文件会让已应用过它的
-- 环境出现"版本缺口";留成注释可以让人看到这次订正的来龙去脉(迁移是历史)。
-- 新库执行本条不会有任何副作用。
SELECT 1;

-- +goose Down
SELECT 1;
