package finalize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
)

// BE-S10-06 验收③:第 4 步"复活/新建"必须**保留 state + size 条件**。
//
// 为什么这条值得一组专门的用例:两个条件各自漏掉的后果都是**静默错数据**,
// 而正常路径（对象确实存在且大小一致）**永远走不到**这些分支:
//   - 漏 `size`:同 hash 但大小不符的行会被"复活"成 live —— 而内容寻址下同 hash
//     必然同内容,出现大小不符只可能是**元数据或对象被破坏**,静默复活等于把一个
//     坏指针重新指给用户,下载时才发现(或更糟:下载到半截内容)。
//   - 漏 `state`:已进入 `deleted`(对象已物理删除)的行被复活 → 文件列表正常、
//     下载 404 的悬空指针。
//
// 所以这里的做法是**注入"行态被改"**:直接在库里把行做成 pending_delete /
// deleting / deleted / 大小不符,再看 bumpObjectRef 落在哪个分支。
// 这是在包内直测(需要调用未导出的 bumpObjectRef);HTTP 层的同类性质由
// finalize 包外的端到端用例覆盖。

func condSetup(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return database
}

// condHash 造一个本用例独有的 hash（前 8 位带用例标记,便于事后排障）。
func condHash(tag string, i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", tag, i, time.Now().Format("150405.000000000"))))
	return hex.EncodeToString(sum[:])
}

// seedObject 直接写一行 file_objects（模拟"上一轮留下的行态"）。
// state='live' 时 ref_count 必须 > 0（表上的不变式约束）。
//
// object_key 刻意用一个**与本次提交不同**的值:复活分支不会改 object_key
// （行还在、对象还在),而"新建/回落"分支会写入新的 key —— 于是"走了哪条分支"
// 可以被直接断言,而不是只看最终状态（两条分支的最终状态恰好可能一样）。
func seedObject(t *testing.T, database *db.DB, hash, key string, size int64, refCount int, state string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state, delete_after)
VALUES ($1, $2, 'fs', $3, $4, $5,
        CASE WHEN $5 = 'live' THEN NULL ELSE now() + interval '24 hours' END)`,
		hash, size, key, refCount, state); err != nil {
		t.Fatalf("预置对象行失败(state=%s ref=%d): %v", state, refCount, err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(),
			`DELETE FROM file_objects WHERE hash_sha256 = $1`, hash)
	})
}

func readObject(t *testing.T, database *db.DB, hash string) (refCount int, size int64, state string, deleteAfter *time.Time) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
SELECT ref_count, size, state, delete_after FROM file_objects WHERE hash_sha256 = $1`,
		hash).Scan(&refCount, &size, &state, &deleteAfter); err != nil {
		t.Fatalf("读对象行失败: %v", err)
	}
	return
}

// 复活分支:pending_delete / deleting 且**大小一致** → 一条 UPDATE 变成 live + 计数+1。
func TestBumpObjectRefRevivesNonLiveStates(t *testing.T) {
	database := condSetup(t)
	ctx := context.Background()
	const size = int64(1024)

	for i, st := range []string{"pending_delete", "deleting"} {
		hash := condHash("revive", i)
		seedObject(t, database, hash, "objects/old-key", size, 0, st) // ref=0 ⇒ 非 live（不变式）

		got, err := bumpObjectRef(ctx, database.Pool, hash, size, "fs", "objects/new-key")
		if err != nil {
			t.Fatalf("state=%s 复活失败: %v", st, err)
		}
		if got.RefCount != 1 || got.State != "live" || got.DeleteAfter != nil {
			t.Fatalf("state=%s 复活后应为 ref=1/live/delete_after=NULL,实际 ref=%d state=%s delete_after=%v",
				st, got.RefCount, got.State, got.DeleteAfter)
		}
		// **走的是复活分支**:行还在、对象还在,key 不该被改写
		if got.ObjectKey != "objects/old-key" {
			t.Fatalf("state=%s 应走复活分支(保留原 key),实际 key=%q", st, got.ObjectKey)
		}
		// 直接读库复核（不信返回值）
		ref, sz, state, del := readObject(t, database, hash)
		if ref != 1 || sz != size || state != "live" || del != nil {
			t.Fatalf("state=%s 库里应为 ref=1/size=%d/live/NULL,实际 ref=%d size=%d state=%s del=%v",
				st, size, ref, sz, state, del)
		}
	}
}

// 新建分支:已 `deleted` 的行不在复活集合里 → 走 INSERT 回落,并被重新置为 live。
//
// ⚠️ 这里**不能**用 object_key 区分两条分支:内容寻址下 key 由 hash 派生,
// 回落分支的 `ON CONFLICT DO UPDATE` 也刻意不动 object_key(它本来就不会变)。
// 所以本用例断言的是**行为**(容忍已删除行并把引用接回去),而"第 4 步是否把
// 已删除行当可用"由下面的 TestUsableObjectStateAndSizeConditions 直接判定。
func TestBumpObjectRefDoesNotReviveDeletedRow(t *testing.T) {
	database := condSetup(t)
	ctx := context.Background()
	const size = int64(2048)
	hash := condHash("deleted", 0)
	seedObject(t, database, hash, "objects/old-key", size, 0, "deleted")

	got, err := bumpObjectRef(ctx, database.Pool, hash, size, "fs", "objects/new-key")
	if err != nil {
		t.Fatalf("deleted 行应按新建/回落处理,实际报错: %v", err)
	}
	if got.RefCount != 1 || got.State != "live" || got.DeleteAfter != nil {
		t.Fatalf("回落新建后应为 ref=1/live/delete_after=NULL,实际 ref=%d state=%s del=%v",
			got.RefCount, got.State, got.DeleteAfter)
	}
}

// **第 4 步"能否复用已有对象"的 state + size 条件**(BE-S10-06 验收③的核心判据)。
//
// 纯函数直测:这条判据决定"复用已有对象"还是"重新写入对象"。两个条件各自漏掉的后果:
//   - 漏 state:已 `deleted`(对象已物理删除)的行被当成可复用 → 文件列表正常、
//     下载 404 的悬空指针;
//   - 漏 size:同 hash 大小不符(只可能来自元数据/对象损坏)被当成可复用 → 用户
//     下到半截内容,而服务端全程无错。
func TestUsableObjectStateAndSizeConditions(t *testing.T) {
	const size = int64(100)
	cases := []struct {
		name  string
		state string
		sz    int64
		want  bool
	}{
		{"live 且大小一致 → 可复用", "live", size, true},
		{"pending_delete 且大小一致 → 可复用(延迟删除窗口内复活)", "pending_delete", size, true},
		{"deleting 且大小一致 → 可复用(worker 已领取但尚未 unlink)", "deleting", size, true},
		{"deleted 且大小一致 → **不可复用**(对象已删除,复用即悬空指针)", "deleted", size, false},
		{"live 但大小不符 → **不可复用**(内容寻址下不可能,说明数据被破坏)", "live", size + 1, false},
		{"pending_delete 但大小不符 → 不可复用", "pending_delete", size - 1, false},
	}
	for _, c := range cases {
		got := usableObject(&model.FileObject{State: c.state, Size: c.sz}, size)
		if got != c.want {
			t.Fatalf("%s: usableObject 应为 %v,实际 %v", c.name, c.want, got)
		}
	}
	if usableObject(nil, size) {
		t.Fatal("对象行不存在时必须判为不可复用")
	}
}

// **size 条件不可省**:同 hash 但大小不符 → 必须显式报错,不得静默复活。
//
// 反向验证方式(手工,记录在此):把 bumpObjectRef 复活语句里的 `AND size = $2`
// 删掉,本用例立刻变成"复活成功且 ref=2"而失败 —— 这正是要实现者看到的那条断言。
func TestBumpObjectRefRejectsSizeMismatch(t *testing.T) {
	database := condSetup(t)
	ctx := context.Background()
	hash := condHash("sizemismatch", 0)
	// 库里的大小与本次不同：内容寻址下这只可能是元数据/对象被破坏
	seedObject(t, database, hash, "objects/old-key", 4096, 3, "live")

	_, err := bumpObjectRef(ctx, database.Pool, hash, 1024, "fs", "objects/key")
	if err == nil {
		t.Fatal("大小不符必须报错,否则会把坏指针静默复活")
	}
	if !strings.Contains(err.Error(), "大小与本次不符") {
		t.Fatalf("错误应明确指出大小不符,实际: %v", err)
	}
	// 原行必须**原封不动**（不能因为一次失败的写入被改坏）
	ref, sz, state, _ := readObject(t, database, hash)
	if ref != 3 || sz != 4096 || state != "live" {
		t.Fatalf("失败路径不得改动原行,实际 ref=%d size=%d state=%s", ref, sz, state)
	}
}

// 反向对照:复活语句里的 state 条件同样不可省 ——
// 若把 `state IN (...)` 去掉,`deleted` 行会被直接复活,上面那条用例的
// "回落新建"断言就会失败(新建与复活的区别正是"有没有重新写入对象")。
// 这里再补一条更直接的:deleted + 大小不符 → 必须报错而不是复活。
func TestBumpObjectRefDeletedWithMismatchStillErrors(t *testing.T) {
	database := condSetup(t)
	ctx := context.Background()
	hash := condHash("deletedmismatch", 0)
	seedObject(t, database, hash, "objects/old-key", 8192, 0, "deleted")

	if _, err := bumpObjectRef(ctx, database.Pool, hash, 4096, "fs", "objects/key"); err == nil {
		t.Fatal("deleted + 大小不符必须报错")
	}
	ref, sz, state, _ := readObject(t, database, hash)
	if ref != 0 || sz != 8192 || state != "deleted" {
		t.Fatalf("失败路径不得改动原行,实际 ref=%d size=%d state=%s", ref, sz, state)
	}
}

// 常规路径:live 行同大小 → 计数+1（这条是"别把正常路径写坏"的对照)。
func TestBumpObjectRefIncrementsLiveRow(t *testing.T) {
	database := condSetup(t)
	ctx := context.Background()
	const size = int64(512)
	hash := condHash("live", 0)
	seedObject(t, database, hash, "objects/old-key", size, 1, "live")

	got, err := bumpObjectRef(ctx, database.Pool, hash, size, "fs", "objects/key")
	if err != nil {
		t.Fatalf("live 行计数失败: %v", err)
	}
	if got.RefCount != 2 || got.State != "live" {
		t.Fatalf("应为 ref=2/live,实际 ref=%d state=%s", got.RefCount, got.State)
	}
}
