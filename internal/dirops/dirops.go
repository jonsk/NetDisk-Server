// Package dirops 实现**目录级异步任务队列**(6.11 超阈值子树操作 / BE-S7-02)。
//
// # 为什么需要异步
//
// 一棵 10 万行的子树在**一个事务**里改完,意味着:①这个事务持有 10 万行锁直到提交,
// 期间任何碰这些行的请求都排队(用户看到的是"整个空间卡住");②连接被占满,
// 并发一高连接池就耗尽;③任何一步失败整棵树回滚,而重试又要从头再来一遍。
// 6.11 因此定下阈值:小规模同步(用户立刻看到结果,也便于前端做乐观更新),
// 超阈值转异步任务并返回 task_id 供轮询。
//
// # 队列形态
//
// 与 TUS/暂存回收同一套:**PG 表 + `FOR UPDATE SKIP LOCKED`**,不引 MQ。
// 任务量级是"每天几十个",引入 MQ 只会多一个需要运维、需要保证投递的组件;
// 而 PG 表的最大好处是**入队与标"移动中"能同一个事务**——两者分开就会出现
// "标了移动中但任务没入队"(条目永久卡住,只能人工修数据)或
// "任务入了队但没标移动中"(任务在跑,用户同时还能往里写)。
//
// # 状态机
//
//	pending ──(领取)──▶ running ──(成功)──▶ done
//	                        │
//	                        ├──(失败)──▶ failed
//	                        └──(租约超时)──▶ pending(下一轮重试)
//
// 与生命周期 worker 同样的自愈思路:`running` 带 `claim_expires_at`,
// 超时即视为 worker 崩溃,由下一轮复位重试。**不追求恰好一次**:
// 执行体自身必须幂等(删除是幂等的;移动用"目标父目录 + 名字"重判,
// 已经移到位就直接成功)。
package dirops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/repo"
)

// DB 与 filesvc.DB 同构(读走池、写走事务)。由 db.Adapter 满足。
type DB interface {
	repo.Querier
	InTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// Kind 是任务类型。
type Kind string

const (
	KindMove   Kind = "move"
	KindDelete Kind = "delete"
)

// State 是任务状态。
type State string

const (
	StatePending State = "pending"
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// IsInflight 表示任务还没结束(用户应当继续轮询)。
func (s State) IsInflight() bool { return s == StatePending || s == StateRunning }

// Task 是一条目录级任务(入队参数 + 执行结果 + 轮询状态)。
type Task struct {
	ID      string
	Kind    Kind
	State   State
	SpaceID string
	// FileID 是子树根。delete 类任务完成后这一行已不存在。
	FileID string
	UserID string
	// NewParentID / NewName 仅 move 类有值
	NewParentID string
	NewName     string
	// 入队时对根行的快照(完成时写聚合事件要用)
	RootVersion  int64
	RootParentID string
	RootName     string
	IsDir        bool
	// 执行结果
	RowsAffected    int64
	FreedBytes      int64
	ReleasedObjects int64
	ErrorCode       string
	ErrorMessage    string
	// ClaimExpiresAt 是领取租约(仅 running 有值)
	ClaimExpiresAt *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	FinishedAt     *time.Time
}

// ErrAlreadyInFlight 表示该子树根上已有一个未完成任务。
var ErrAlreadyInFlight = errors.New("dirops: 该目录上已有未完成的目录操作")

// ErrNotFound 表示任务不存在。
var ErrNotFound = errors.New("dirops: 任务不存在")

// ErrNoClaim 表示没有领取到任务(队列为空)。
var ErrNoClaim = errors.New("dirops: 队列为空")

// Queue 是任务表的读写入口。
type Queue struct {
	DB DB
	// SyncMaxRows 只用于轮询接口回显(执行体在 filesvc 里判阈值),
	// 这里不参与逻辑。
	SyncMaxRows int
}

// taskColumns 是查询任务的列清单(顺序与 scanTask 一致)。
const taskColumns = `
  id, kind, state, space_id, file_id, user_id,
  coalesce(new_parent_id::text, ''), coalesce(new_name, ''),
  root_version, coalesce(root_parent_id::text, ''), coalesce(root_name, ''),
  is_dir, rows_affected, freed_bytes, released_objects,
  coalesce(error_code, ''), coalesce(error_message, ''),
  claim_expires_at, created_at, updated_at, finished_at`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	if err := row.Scan(
		&t.ID, &t.Kind, &t.State, &t.SpaceID, &t.FileID, &t.UserID,
		&t.NewParentID, &t.NewName,
		&t.RootVersion, &t.RootParentID, &t.RootName,
		&t.IsDir, &t.RowsAffected, &t.FreedBytes, &t.ReleasedObjects,
		&t.ErrorCode, &t.ErrorMessage,
		&t.ClaimExpiresAt, &t.CreatedAt, &t.UpdatedAt, &t.FinishedAt,
	); err != nil {
		return nil, err
	}
	return &t, nil
}

// Create 入队一个任务,并在**同一事务**里把子树根标成"移动中"。
//
// 两件事必须同事务:入队成功但标记失败 → 用户能继续往一棵正在被操作的树里写;
// 标记成功但入队失败 → 条目永久停在"移动中",只有人工改库才能恢复。
func (q *Queue) Create(ctx context.Context, t *Task) (*Task, error) {
	if q.DB == nil {
		return nil, errors.New("dirops: DB 未装配")
	}
	var out *Task
	err := q.DB.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
INSERT INTO dir_op_tasks
   (kind, space_id, file_id, user_id, new_parent_id, new_name,
    root_version, root_parent_id, root_name, is_dir)
VALUES ($1, $2, $3, $4, nullif($5,'')::uuid, nullif($6,''),
        $7, nullif($8,'')::uuid, nullif($9,''), $10)
RETURNING `+taskColumns,
			string(t.Kind), t.SpaceID, t.FileID, t.UserID,
			t.NewParentID, t.NewName,
			t.RootVersion, t.RootParentID, t.RootName, t.IsDir)
		created, err := scanTask(row)
		if err != nil {
			if repo.IsUniqueViolation(err) {
				// 唯一索引 dir_op_tasks_inflight_key:同根已有在飞任务
				return ErrAlreadyInFlight
			}
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE files SET op_state = 'moving', updated_at = now() WHERE id = $1`,
			t.FileID); err != nil {
			return err
		}
		out = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Get 按 id 读任务(轮询接口)。
func (q *Queue) Get(ctx context.Context, id string) (*Task, error) {
	if q.DB == nil {
		return nil, errors.New("dirops: DB 未装配")
	}
	t, err := scanTask(q.DB.QueryRow(ctx,
		`SELECT `+taskColumns+` FROM dir_op_tasks WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return t, nil
}

// ListInflight 返回某子树根上未完成的任务(运维/测试用)。
func (q *Queue) ListInflight(ctx context.Context, fileID string) ([]*Task, error) {
	if q.DB == nil {
		return nil, errors.New("dirops: DB 未装配")
	}
	rows, err := q.DB.Query(ctx,
		`SELECT `+taskColumns+` FROM dir_op_tasks
		  WHERE file_id = $1 AND state IN ('pending','running')`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, serr := scanTask(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ResetStale 把租约超时的 running 任务退回 pending(worker 崩溃自愈)。
//
// 与 lifecycle.ResetStaleClaims 同思路:**必须能自愈**,否则一次进程崩溃
// 就让一个目录永久停在"移动中"(用户看到的是"这个目录再也动不了了",
// 而且没有任何报错指向原因)。
func (q *Queue) ResetStale(ctx context.Context) (int64, error) {
	if q.DB == nil {
		return 0, errors.New("dirops: DB 未装配")
	}
	tag, err := q.DB.Exec(ctx, `
UPDATE dir_op_tasks
   SET state = 'pending', claim_expires_at = NULL, updated_at = now()
 WHERE state = 'running' AND claim_expires_at IS NOT NULL AND claim_expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Claim 领取一个待执行任务(标记 running + 续租)。
//
// `FOR UPDATE SKIP LOCKED` + 单条 UPDATE ... RETURNING:多 worker 并发时
// 不互相等待、也不会重复领取(与 lifecycle.claim 同款)。
// 单条语句领取比"SELECT 后 UPDATE"少一个竞态窗口。
func (q *Queue) Claim(ctx context.Context, lease time.Duration) (*Task, error) {
	if q.DB == nil {
		return nil, errors.New("dirops: DB 未装配")
	}
	if lease <= 0 {
		lease = 10 * time.Minute
	}
	t, err := scanTask(q.DB.QueryRow(ctx, `
UPDATE dir_op_tasks SET state = 'running',
       claim_expires_at = now() + $1::interval, updated_at = now()
 WHERE id = (
   SELECT id FROM dir_op_tasks
    WHERE state = 'pending'
    ORDER BY created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED)
RETURNING `+taskColumns, intervalLiteral(lease)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoClaim
		}
		return nil, err
	}
	return t, nil
}

// Outcome 是执行体回报的结果。
type Outcome struct {
	RowsAffected    int64
	FreedBytes      int64
	ReleasedObjects int64
}

// Finish 标记任务成功,并清除子树根的"移动中"标记。
//
// 清标记用 `op_state = ''` 的**无条件**写法(不校验当前值):任务成功就意味着
// 它标的那次操作已经结束;若期间被人手工改过,以任务结论为准。
// 清标记与标 done 同事务 —— 否则崩在中间会让条目永久停在"移动中"。
func (q *Queue) Finish(ctx context.Context, t *Task, out Outcome) error {
	if q.DB == nil {
		return errors.New("dirops: DB 未装配")
	}
	return q.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
UPDATE dir_op_tasks
   SET state = 'done', rows_affected = $2, freed_bytes = $3, released_objects = $4,
       claim_expires_at = NULL, finished_at = now(), updated_at = now()
 WHERE id = $1`,
			t.ID, out.RowsAffected, out.FreedBytes, out.ReleasedObjects); err != nil {
			return err
		}
		// delete 类任务完成后根行已不存在,这条 UPDATE 影响 0 行是正常的
		_, err := tx.Exec(ctx,
			`UPDATE files SET op_state = '', updated_at = now() WHERE id = $1`, t.FileID)
		return err
	})
}

// Fail 标记任务失败,并清除"移动中"标记(否则条目永久动不了)。
func (q *Queue) Fail(ctx context.Context, t *Task, code, message string) error {
	if q.DB == nil {
		return errors.New("dirops: DB 未装配")
	}
	return q.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
UPDATE dir_op_tasks
   SET state = 'failed', error_code = $2, error_message = $3,
       claim_expires_at = NULL, finished_at = now(), updated_at = now()
 WHERE id = $1`, t.ID, code, message); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE files SET op_state = '', updated_at = now() WHERE id = $1`, t.FileID)
		return err
	})
}

// Execute 是执行体:由 filesvc 实现(它才知道怎么改元数据、引用计数与变更流)。
//
// 用函数类型而不是接口:只有一个方法,接口只会多一层命名。
type Execute func(ctx context.Context, t *Task) (Outcome, error)

// ExecError 让执行体回传一个**稳定的错误码**给客户端(客户端按 code 分流,
// 不解析文案)。
type ExecError struct {
	Code    string
	Message string
	Err     error
}

func (e *ExecError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *ExecError) Unwrap() error { return e.Err }

// Worker 驱动队列。同样做成"一轮一函数",便于定时任务复用与测试精确驱动。
type Worker struct {
	Queue *Queue
	Exec  Execute
	Log   *slog.Logger
	// Lease 是单任务的领取租约(默认 10min)
	Lease time.Duration
}

func (w *Worker) lease() time.Duration {
	if w.Lease > 0 {
		return w.Lease
	}
	return 10 * time.Minute
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// RunOnce 复位超时任务后执行**一个**任务;队列为空返回 (false, nil)。
//
// 一轮只做一个:一个目录任务可能是十万行的写,两个并发执行并不会更快
// (它们争同一批行锁),反而更容易把连接池占满。
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if w.Queue == nil || w.Exec == nil {
		return false, errors.New("dirops: Queue/Exec 未装配")
	}
	if n, err := w.Queue.ResetStale(ctx); err != nil {
		return false, err
	} else if n > 0 {
		w.log().Warn("dirops: 复位租约超时的任务(worker 可能崩溃过)", "count", n)
	}

	t, err := w.Queue.Claim(ctx, w.lease())
	if err != nil {
		if errors.Is(err, ErrNoClaim) {
			return false, nil
		}
		return false, err
	}

	out, xerr := w.Exec(ctx, t)
	if xerr != nil {
		code, msg := "internal", xerr.Error()
		var ee *ExecError
		if errors.As(xerr, &ee) {
			code, msg = ee.Code, ee.Message
		}
		if ferr := w.Queue.Fail(ctx, t, code, msg); ferr != nil {
			return true, fmt.Errorf("dirops: 标记任务失败时出错: %w", ferr)
		}
		w.log().Error("dirops: 任务执行失败",
			"task_id", t.ID, "kind", string(t.Kind), "code", code, "err", xerr)
		return true, nil
	}
	if ferr := w.Queue.Finish(ctx, t, out); ferr != nil {
		return true, fmt.Errorf("dirops: 标记任务完成时出错: %w", ferr)
	}
	w.log().Info("dirops: 任务完成",
		"task_id", t.ID, "kind", string(t.Kind), "rows", out.RowsAffected)
	return true, nil
}

// intervalLiteral 把 Duration 转成 PG interval 字面量(见 lifecycle 同函数说明:
// 值全部来自服务端配置,只保留整数秒)。
func intervalLiteral(d time.Duration) string {
	secs := int64(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%d seconds", secs)
}
