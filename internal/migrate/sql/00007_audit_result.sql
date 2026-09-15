-- +goose Up
-- 审计的**结果分类**(4.4 / BE-S10-01)。
--
-- 原先只落 `result_code`(稳定业务码,如 forbidden / not_found / cursor_expired)。
-- 业务码更精确,但管理后台最常见的查询是"**被拒绝**的访问有哪些" ——
-- 用业务码表达它需要一张码→分类的映射表,而映射表会随新业务码漂移
-- (新码忘了登记 → 那条记录在"被拒绝"视图里永远不出现,而这正是安全审计最怕的漏)。
--
-- 因此把分类**在写入时就定下来**(audit.Record.Result):'' = 成功、
-- denied / not_found / conflict / failed。分类由 handler 按 HTTP 语义判定,
-- 与 apierr 的状态码同源,不会漏。
ALTER TABLE audit_logs ADD COLUMN result varchar(16) NOT NULL DEFAULT '';

-- 管理后台的主查询之一是"按结果分类 + 时间倒序"(被拒绝的访问、失败的下载)
CREATE INDEX audit_logs_result_time_idx ON audit_logs (result, created_at DESC);

-- +goose Down
DROP INDEX audit_logs_result_time_idx;
ALTER TABLE audit_logs DROP COLUMN result;
