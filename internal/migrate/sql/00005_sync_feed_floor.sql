-- +goose Up
-- sync_feed 的**保留水位**(BE-S9-01/03):清理任务删掉旧行后,必须留下
-- "这个空间的行已经从哪一条开始被删了"的痕迹 —— 否则一个过老的游标
-- (since 远小于现存最小 change_seq)会查不到任何行、**静默返回空**,
-- 客户端以为自己已经同步完,而实际上它漏掉了一整段变更。
--
-- 用 spaces 上的列而不是新建表:它是每空间恰好一行一值的水位,
-- 与 last_seq(计数器)同处一行,读放大与一致性都最好(同一次
-- SELECT 就能同时拿到"当前最大 seq"与"已清理到哪")。
ALTER TABLE spaces ADD COLUMN feed_floor_seq bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE spaces DROP COLUMN feed_floor_seq;
