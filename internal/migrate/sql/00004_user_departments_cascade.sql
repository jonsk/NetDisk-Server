-- +goose Up
-- 部门成员关联的删除语义改为 CASCADE(4.2 组织架构)。
--
-- 背景:`user_departments.department_id` 原为 ON DELETE RESTRICT,导致
-- **部门一旦被任何用户关联就永远删不掉** —— 而这正是组织同步的常规操作
-- (源端删掉一个部门后本地要跟随)。原约束把"删除部门"变成了一个
-- 需要人工先拆关联的运维动作,与"同步任务可全量重跑"的设计目标冲突。
--
-- 为什么 CASCADE 在这里是安全的(而不是随意的 ON DELETE 选择):
--   * `user_departments` 是**纯关联表**,不承载业务数据 ——
--     删掉一行只表示"某人不再属于某部门",没有信息丢失;
--   * 它不参与配额/权限的持久化统计(团队空间的判权走 `space_members`) ——
--     因此级联删除不会造成账实不符;
--   * 与之相对,`users` 侧仍保持 RESTRICT:`user_departments.user_id` 不变,
--     删用户前必须先显式清理关联,避免误删用户时静默带走组织信息。
--
-- 闭包表 `department_closure` 两侧本来就都是 CASCADE,无需改动。

ALTER TABLE user_departments
    DROP CONSTRAINT user_departments_department_id_fkey;
ALTER TABLE user_departments
    ADD CONSTRAINT user_departments_department_id_fkey
    FOREIGN KEY (department_id) REFERENCES departments(id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE user_departments
    DROP CONSTRAINT user_departments_department_id_fkey;
ALTER TABLE user_departments
    ADD CONSTRAINT user_departments_department_id_fkey
    FOREIGN KEY (department_id) REFERENCES departments(id) ON DELETE RESTRICT;
