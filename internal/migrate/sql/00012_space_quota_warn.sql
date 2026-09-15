-- +goose Up
-- 配额预警阈值(4.3:80% 通知、95% 禁传)落到**空间行**上。
--
-- 为什么给"预警"单开一列,而 95% 禁传仍是全局策略:
--   - 95% 是**安全阀**(再传就真超了),它必须各处一致,改它要有全局影响评估,
--     所以留在 config(单一来源);
--   - 80% 是**提醒**,不同空间的合理值不同(临时项目空间希望早点提醒,
--     长期归档空间希望少打扰),所以允许按空间覆盖 —— 这正是管理后台
--     "设置预警阈值"这个动作真正需要的东西。
--
-- 默认 80 与文档一致;CHECK 限制在 1..100:0 会让"预警"永远不触发
-- (而管理员以为自己设了),>100 则永远触发(通知疲劳)。
ALTER TABLE spaces
    ADD COLUMN IF NOT EXISTS quota_warn_percent int NOT NULL DEFAULT 80;

-- 约束没有 IF NOT EXISTS 语法,而本项目的迁移会被并行的测试包重复执行,
-- 因此包一层 DO 块自己判存在。goose 按分号切语句,函数/DO 体必须用
-- StatementBegin/End 包裹(否则 $$ 里的分号会把语句截断)。
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'spaces_quota_warn_chk') THEN
        ALTER TABLE spaces
            ADD CONSTRAINT spaces_quota_warn_chk
            CHECK (quota_warn_percent > 0 AND quota_warn_percent <= 100);
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE spaces DROP CONSTRAINT IF EXISTS spaces_quota_warn_chk;
ALTER TABLE spaces DROP COLUMN IF EXISTS quota_warn_percent;
