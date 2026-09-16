-- +goose Up
-- 目录级异步任务队列(BE-S7-02 / 6.11)。
--
-- 6.11 定的规则:子树操作 ≤1000 行(阈值可配)走同步事务;超阈值转异步任务,
-- 返回 task_id 供轮询。没有这张表,"超阈值"就只能靠"把事务开到把连接池占满"
-- 或"直接拒绝"——前者会让一次删 10 万文件把整个服务的连接吃光,
-- 后者让用户根本无法删除大目录。
--
-- 队列形态与 TUS/暂存回收同一套:**PG 表 + FOR UPDATE SKIP LOCKED**(不引 MQ)。
-- 理由与那里一致:量级是"每天几十个任务",引入 MQ 只增加一个需要运维、
-- 需要保证不丢消息的组件,而 PG 表天然与业务数据同事务(入队与标"移动中"必须同事务)。

-- files.op_state:子树根在异步任务期间的**状态标记**(6.11 "标移动中")。
-- 放在 files 上而不是任务表里,是因为每个读写请求都要判"这个条目能不能动",
-- 而请求手上正好有文件 id —— 放任务表会让每次读都多一次 JOIN/子查询。
ALTER TABLE files ADD COLUMN op_state varchar(16) NOT NULL DEFAULT '';
ALTER TABLE files ADD CONSTRAINT files_op_state_chk CHECK (op_state IN ('', 'moving'));
-- 部分索引:op_state 非空的行在生产里永远是"个位数",全表索引纯属浪费
CREATE INDEX files_op_state_idx ON files (id) WHERE op_state <> '';

CREATE TABLE dir_op_tasks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind            varchar(16) NOT NULL CHECK (kind IN ('move', 'delete')),
    state           varchar(16) NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending', 'running', 'done', 'failed')),
    space_id        uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    -- 子树根。**刻意不加外键**:delete 类任务会把这一行删掉,
    -- 而任务行必须留下来(它是"这次操作发生过、结果如何"的唯一记录)。
    file_id         uuid NOT NULL,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    -- move 参数(delete 类为 NULL)
    new_parent_id   uuid,
    new_name        varchar(255),
    -- 入队时对根行的快照:任务完成时要写 moved 聚合事件,而那时行可能已被
    -- 移动/删除;没有快照就只能编一个 version(客户端会据此丢掉这条事件)
    root_version    bigint NOT NULL DEFAULT 0,
    root_parent_id  uuid,
    root_name       varchar(255),
    is_dir          boolean NOT NULL DEFAULT false,
    -- 执行结果
    rows_affected   bigint NOT NULL DEFAULT 0,
    freed_bytes     bigint NOT NULL DEFAULT 0,
    released_objects bigint NOT NULL DEFAULT 0,
    error_code      varchar(64),
    error_message   text,
    -- 领取租约:worker 崩溃后由下一轮复位,否则任务永远停在 running
    claim_expires_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);

-- 领取用:只扫未完成的那一小撮
CREATE INDEX dir_op_tasks_claim_idx ON dir_op_tasks (created_at)
    WHERE state IN ('pending', 'running');
-- **同一子树根只允许一个在飞任务**:用唯一索引而不是"先查再插" ——
-- 后者在并发下必然漏(两个请求同时查、同时插),而漏掉的那个会让
-- 两个 worker 同时改同一棵子树(引用计数翻倍/深度错乱)。
CREATE UNIQUE INDEX dir_op_tasks_inflight_key ON dir_op_tasks (file_id)
    WHERE state IN ('pending', 'running');

-- +goose Down
DROP TABLE dir_op_tasks;
DROP INDEX files_op_state_idx;
ALTER TABLE files DROP CONSTRAINT files_op_state_chk;
ALTER TABLE files DROP COLUMN op_state;
