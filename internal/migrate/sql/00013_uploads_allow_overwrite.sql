-- +goose Up
-- v13:上传任务记住"这次是否要覆盖同名文件"。
--
-- 为什么必须落库(不能只放在内存/请求里):TUS 是可续传的,客户端可能在"建任务"
-- 之后隔很久(甚至跨进程重启)才把最后一片传完并触发定稿。定稿那一刻若不知道当初
-- 的覆盖意图,就只能按默认语义回 409 name_conflict —— 于是"改本地大文件"会在
-- 最后一片才失败,前面的字节全白传。
--
-- 默认 false:保持"绝不静默覆盖"的原语义(见 finalize.Input.AllowOverwrite 的说明)。
-- IF NOT EXISTS 不是随手加的:v13 曾经被误写成"Up 段为空、DDL 落在 Down 段",
-- goose 照样记账成"已应用到 13",于是有的环境(生产 10.14.37.187)版本号到了 13
-- 却没有这一列,而有的环境是人工 psql 补的列。加上 IF NOT EXISTS 后,这两种环境
-- 重放本条都能收敛到同一状态,新库也照常建列。
ALTER TABLE uploads ADD COLUMN IF NOT EXISTS allow_overwrite BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE uploads DROP COLUMN IF EXISTS allow_overwrite;
