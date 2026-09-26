-- +goose Up
-- ============================================================================
-- 00016_default_org_root.sql
--   1. 确保系统有一个机构根节点(parent_id IS NULL)
--   2. 确保根节点的闭包自指行存在
--   3. 清理孤儿闭包行(历史手工删 departments 可能残留)
--   4. 新增 user_departments(department_id) 单列索引
--
-- ⚠ 只种子、不收编:已有根的生产库不碰(避免覆盖真实机构名)。
--    守卫用 WHERE NOT EXISTS (parent_id IS NULL) 而非固定 UUID ——
--    否则已有根的生产库会被插入第二个根,合并页"单根"假设崩塌。
-- ⚠ 种子名用"我的机构",仅空库/全新部署走种子分支。
-- ============================================================================

-- 种子根节点:仅当当前没有任何根(parent_id IS NULL)时才插入。
INSERT INTO departments (parent_id, name, sort_order, source)
SELECT NULL, '我的机构', 0, 'manual'
WHERE NOT EXISTS (SELECT 1 FROM departments WHERE parent_id IS NULL);

-- 闭包行:确保根节点的自引用行存在(不管它是哪条)。
INSERT INTO department_closure (ancestor_id, descendant_id, depth)
SELECT id, id, 0 FROM departments WHERE parent_id IS NULL
ON CONFLICT (ancestor_id, descendant_id) DO NOTHING;

-- 清理孤儿闭包行(其他环境若手动删过 departments 行会残留)。
DELETE FROM department_closure
 WHERE ancestor_id NOT IN (SELECT id FROM departments)
    OR descendant_id NOT IN (SELECT id FROM departments);

-- 性能索引:user_departments 的 PK 是 (user_id, department_id),按 department_id
-- 单列查(仅本部门筛选、CountUsersInDept 删除守卫)不走该 PK 索引。
-- 与 department_closure_desc_idx(descendant_id) 对称。
CREATE INDEX IF NOT EXISTS user_departments_dept_idx
    ON user_departments (department_id);

-- +goose Down
-- 不可逆:不回退根节点的插入(回退会让机构管理页无根可用)。
-- goose down 会跳过本迁移,不报错。
SELECT 1;
