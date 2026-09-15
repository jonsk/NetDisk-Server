-- +goose Up
-- 组织同步状态(BE-S2-07 验收①④)。
--
-- 每行一个 IdP(kind):游标(断点续传)、最近一次运行的结果与计数。
--
-- 为什么把**游标**放在库里而不是内存:验收①要求"全量分批 + 断点续传" ——
-- 中断可能来自进程重启/发布/外部 API 限流,而"上次同步到哪"必须活过重启;
-- 放内存的话,一次发布就让几万人的同步从头再来(外部 API 的限流会把它再打断一次)。
--
-- 为什么把**计数与错误**也放在同一行:验收④要求"同步任务状态可查" ——
-- 管理员要回答的是"同步跑到哪了、上一次成没成、有没有漂移",这些必须在
-- 没有 Redis / 没有日志的情况下也能查到(单机部署里日志常常已经轮转掉了)。
CREATE TABLE IF NOT EXISTS idp_sync_state (
    kind            varchar(32) PRIMARY KEY,
    -- cursor 是提供方给的续传游标(为空表示全量已完成/从未开始)
    cursor          varchar(256) NOT NULL DEFAULT '',
    -- 一次"全量"是否已跑完(与 cursor 配合:中途中断时 cursor 非空、full_done=false)
    full_done       boolean NOT NULL DEFAULT false,
    last_run_at     timestamptz,
    last_ok         boolean NOT NULL DEFAULT true,
    last_error      text NOT NULL DEFAULT '',
    pages_done      int NOT NULL DEFAULT 0,
    depts_upserted  bigint NOT NULL DEFAULT 0,
    users_created   bigint NOT NULL DEFAULT 0,
    users_updated   bigint NOT NULL DEFAULT 0,
    memberships_set bigint NOT NULL DEFAULT 0,
    users_disabled  bigint NOT NULL DEFAULT 0,
    -- 对账漂移(验收③):与提供方快照比,本地多/少的人与部门
    drift_users     bigint NOT NULL DEFAULT 0,
    drift_depts     bigint NOT NULL DEFAULT 0,
    last_reconcile_at timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE idp_sync_state;
