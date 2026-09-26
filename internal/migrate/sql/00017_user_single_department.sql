-- +goose Up
-- ============================================================================
-- 00017_user_single_department.sql
--   单部门归属:一个用户只能属于 1 个机构(对应"机构与人员"页强约束)。
--   1. 先去重:每个用户只保留 is_primary 的那条;若无 primary 则保留
--      department_id 最小的一条(历史多归属数据收敛为单条)。
--   2. 加 UNIQUE(user_id) 约束,从数据库层杜绝日后任何途径写入多部门。
-- ============================================================================

-- 去重:删除每个用户"非保留"的多余行。保留行的选取:
--   ORDER BY user_id, is_primary DESC, department_id
--   → DISTINCT ON (user_id) 取每个用户的第一行(优先 primary,其次 id 最小)。
DELETE FROM user_departments
WHERE ctid NOT IN (
    SELECT DISTINCT ON (user_id) ctid
    FROM user_departments
    ORDER BY user_id, is_primary DESC, department_id
);

-- 强约束:一个用户最多 1 条部门归属。
ALTER TABLE user_departments ADD CONSTRAINT uniq_user_department UNIQUE (user_id);

-- +goose Down
ALTER TABLE user_departments DROP CONSTRAINT IF EXISTS uniq_user_department;
