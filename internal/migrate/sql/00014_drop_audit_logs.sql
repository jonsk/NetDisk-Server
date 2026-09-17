-- +goose Up
-- 业务审计(查询/导出)已随开源精简移除(handlers_audit.go 中 auditAuth/auditAction 为
-- no-op 占位,后端不再写入 audit_logs)。契约已删除 /api/v1/audit/logs。
-- 此处统一把 audit_logs 表及全部索引/按月分区用 CASCADE 移除,保持 schema 与社区版
-- 实际能力一致。表与数据均不再需要,直接 DROP(如先前已部署并留有历史数据,请先备份)。
DROP TABLE IF EXISTS audit_logs CASCADE;

-- +goose Down
-- 社区版不再提供审计能力,不回建此表。
SELECT 1;
