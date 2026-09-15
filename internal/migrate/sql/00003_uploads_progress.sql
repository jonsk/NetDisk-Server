-- +goose Up
-- 为 TUS 断点续传记录已落盘字节数(6.10)。
--
-- 为什么不从临时文件 stat 得到?—— 临时文件可能被运维误删、可能还没 flush,
-- 而 HEAD 给出错误的 offset 会让客户端**写到错误的位置**(数据损坏而非失败),
-- 所以进度必须在与 uploads 行同一事务里持久化,是唯一真相。
ALTER TABLE uploads
    ADD COLUMN uploaded_bytes bigint NOT NULL DEFAULT 0
    CHECK (uploaded_bytes >= 0);

-- 不变式:已接收字节不能超过声明大小(声明大小在创建后不可变)
ALTER TABLE uploads
    ADD CONSTRAINT uploads_uploaded_le_declared CHECK (uploaded_bytes <= declared_size);

-- +goose Down
ALTER TABLE uploads DROP CONSTRAINT IF EXISTS uploads_uploaded_le_declared;
ALTER TABLE uploads DROP COLUMN IF EXISTS uploaded_bytes;
