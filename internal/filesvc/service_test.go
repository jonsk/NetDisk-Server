package filesvc_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// 文件元数据的真实 PG 集成测试(BE-S4-05)。
// 未设置 NETDISK_TEST_DSN 时全部 skip。

type fixture struct {
	svc      *filesvc.Service
	pool     *pgxpool.Pool
	database *db.DB
	store    *storage.FS
	userID   string
	spaceID  string
	rootID   string
	// storeRoot 是本用例独占的存储根目录 —— 用于"内容是否真的被复制"这类
	// 需要数物理文件个数的断言(内容寻址库里同 hash 只有一个物理文件)。
	storeRoot string
	// made 记录本用例造出的对象 hash(用于精确清理;绝不使用全表 DELETE)
	made []string
}

// trackHash 登记本用例造出的对象 hash。
func (f *fixture) trackHash(hash string) { f.made = append(f.made, hash) }

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	suffix := time.Now().Format("150405.000000000")
	users := repo.UserRepo{}
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := users.Create(ctx, tx, repo.CreateInput{
			Username: "fs_" + suffix, Email: "fs_" + suffix + "@example.com",
			DisplayName: "文件用例测试用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	// 注意:真正的 cleanup 在 fixture 构造后注册(见 installCleanup)——
	// 它要用 fx.made 记录的对象 hash,因此必须等 fixture 造好。

	spaces := repo.SpaceRepo{}
	sp, err := spaces.PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	files := repo.FileRepo{}
	root, err := files.GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}
	storeRoot := t.TempDir()
	store, err := storage.NewFS(storeRoot)
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	fx := &fixture{
		svc: &filesvc.Service{
			Spaces: spaces, Files: files, DB: db.AsQuerier(database),
			Name: namepolicy.Default(),
		},
		pool: database.Pool, database: database, store: store,
		userID: u.ID, spaceID: sp.ID, rootID: root.ID,
		storeRoot: storeRoot,
	}
	installCleanup(t, fx)
	return fx
}

// installCleanup 注册精确清理。
//
// 三条限定,没有任何无条件 DELETE:
//  1. 本用例显式造出的对象(made 清单 —— 它们没有 files 行指向)
//  2. 本用例空间内文件引用的对象
//  3. 本用例的文件/空间/用户
//
// 踩过的坑:写 `DELETE FROM file_objects WHERE ref_count <= 0` 这种全表条件
// 会删掉**别的测试包**正在断言的对象(表现为"读对象行失败: no rows",
// 而单独跑必过)。清理必须带明确限定。
func installCleanup(t *testing.T, fx *fixture) {
	t.Helper()
	t.Cleanup(func() {
		bg := context.Background()
		if len(fx.made) > 0 {
			_, _ = fx.database.Pool.Exec(bg,
				`DELETE FROM file_objects WHERE hash_sha256 = ANY($1::varchar[])`, fx.made)
		}
		_, _ = fx.database.Pool.Exec(bg, `
DELETE FROM file_objects WHERE hash_sha256 IN (
  SELECT DISTINCT hash_sha256 FROM files
   WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL)`, fx.userID)
		_, _ = fx.database.Pool.Exec(bg,
			`DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, fx.userID)
		_, _ = fx.database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, fx.userID)
		_, _ = fx.database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, fx.userID)
	})
}

// mkFile 直接在库里插一行文件(绕开 finalize —— 本组用例只测元数据读取)。
func (f *fixture) mkFile(t *testing.T, parent, name string, size int64, version int64) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, version, depth, hash_sha256, mime_type)
VALUES ($1, $2, $3, $4, false, $5, $6, 1, $7, 'application/octet-stream')
RETURNING id`,
		f.spaceID, parent, f.userID, name, size, version, fmt.Sprintf("%064x", len(name)+int(size))).Scan(&id)
	if err != nil {
		t.Fatalf("造文件 %s 失败: %v", name, err)
	}
	return id
}

func (f *fixture) mkDir(t *testing.T, parent, name string) string {
	t.Helper()
	d, err := repo.FileRepo{}.CreateDir(context.Background(), f.pool, repo.CreateDirInput{
		SpaceID: f.spaceID, ParentID: parent, OwnerID: f.userID, Name: name, Depth: 1,
	})
	if err != nil {
		t.Fatalf("造目录 %s 失败: %v", name, err)
	}
	return d.ID
}

// ---- 列表 ----

func TestListReturnsEntriesWithRequiredFields(t *testing.T) {
	f := setup(t)
	f.mkFile(t, f.rootID, "b.txt", 20, 3)
	f.mkFile(t, f.rootID, "a.txt", 10, 1)
	f.mkDir(t, f.rootID, "docs")

	res, err := f.svc.List(context.Background(), filesvc.ListInput{UserID: f.userID, SpaceID: f.spaceID})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("应有 3 个条目,实际 %d", len(res.Entries))
	}
	// 排序:lower(name) 升序 → a.txt, b.txt, docs
	if res.Entries[0].Name != "a.txt" || res.Entries[1].Name != "b.txt" || res.Entries[2].Name != "docs" {
		t.Fatalf("应按名称升序返回,实际 %v", names(res.Entries))
	}
	e := res.Entries[0]
	// 6.4:必须带 version/etag/size/mtime/mime
	if e.Version != 1 || e.Size != 10 || e.MimeType == "" || e.UpdatedAt == "" {
		t.Fatalf("条目缺少必需字段: %+v", e)
	}
	if e.ETag != `"00000001-`+e.HashSHA256[:8]+`"` {
		t.Fatalf("ETag 应由 version + hash 前 8 位组成且带引号,实际 %q (hash=%s)", e.ETag, e.HashSHA256)
	}
	if !res.Entries[2].IsDir {
		t.Fatal("docs 应标记为目录")
	}
}

// space_id 与 parent_id 均可省略:默认落到调用者的个人空间根目录。
func TestListDefaultsToPersonalRoot(t *testing.T) {
	f := setup(t)
	f.mkFile(t, f.rootID, "x.txt", 1, 1)

	res, err := f.svc.List(context.Background(), filesvc.ListInput{UserID: f.userID})
	if err != nil {
		t.Fatalf("省略 space/parent 应可用: %v", err)
	}
	if res.SpaceID != f.spaceID || res.ParentID != f.rootID {
		t.Fatalf("应解析到个人空间根目录,实际 space=%s parent=%s", res.SpaceID, res.ParentID)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("应有 1 个条目,实际 %d", len(res.Entries))
	}
}

// keyset 分页:连续翻页不重不漏。**故意用 OFFSET 会漏的方案验证**:
// 翻页途中插入新条目,keyset 不会重复返回已看过的条目。
func TestListKeysetPagination(t *testing.T) {
	f := setup(t)
	// 造 7 个文件:c00..c06
	for i := 0; i < 7; i++ {
		f.mkFile(t, f.rootID, fmt.Sprintf("c%02d.txt", i), int64(i), 1)
	}
	ctx := context.Background()

	seen := []string{}
	after := ""
	pages := 0
	for {
		res, err := f.svc.List(ctx, filesvc.ListInput{
			UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, After: after, Limit: 3,
		})
		if err != nil {
			t.Fatalf("第 %d 页失败: %v", pages, err)
		}
		pages++
		for _, e := range res.Entries {
			seen = append(seen, e.Name)
		}
		if res.NextAfter == "" {
			break
		}
		if pages > 10 {
			t.Fatal("分页未收敛(可能游标未推进)")
		}
		after = res.NextAfter

		// 第一页之后插入一个新条目(名在 c 之前),keyset 不应因此重复返回旧条目
		if pages == 1 {
			f.mkFile(t, f.rootID, "b-new.txt", 100, 1)
		}
	}
	if len(seen) != 7 {
		t.Fatalf("应恰好遍历 7 条(不重不漏),实际 %d: %v", len(seen), seen)
	}
	uniq := map[string]bool{}
	for _, n := range seen {
		if uniq[n] {
			t.Fatalf("分页出现重复条目 %s: %v", n, seen)
		}
		uniq[n] = true
	}
	// 顺序必须单调
	for i := 1; i < len(seen); i++ {
		if seen[i-1] >= seen[i] {
			t.Fatalf("分页顺序不单调: %v", seen)
		}
	}
}

// ---- 权限过滤(4.3)----

// 别人的个人空间:必须 403/410,且**不返回任何条目**。
func TestListRejectsForeignPersonalSpace(t *testing.T) {
	f := setup(t)
	other := setup(t)
	other.mkFile(t, other.rootID, "secret.txt", 1, 1)

	_, err := f.svc.List(context.Background(), filesvc.ListInput{
		UserID: f.userID, SpaceID: other.spaceID, ParentID: other.rootID,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("跨空间读取应失败,得到 %v", err)
	}
	if ae.Status != 403 {
		t.Fatalf("应 403,实际 %d(%s)", ae.Status, ae.Code)
	}
	// 关键:用 space_revoked 而不是 not_found —— 客户端据此"不删本地"(7.2 P1-2)
	if ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("应返回 space_revoked(客户端据此不删本地),实际 %s", ae.Code)
	}
}

// 冻结空间:写操作被拒、读也拒绝(冻结语义是"整空间只读封锁")。
func TestListRejectsFrozenSpace(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE spaces SET frozen = true WHERE id = $1`, f.spaceID); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	_, err := f.svc.List(context.Background(), filesvc.ListInput{UserID: f.userID, SpaceID: f.spaceID})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("冻结空间应 space_revoked,得到 %v", err)
	}
}

// 只读成员可以读(4.3:reader 有读权限)
func TestListAllowsReader(t *testing.T) {
	f := setup(t)
	owner := setup(t)
	teamID, teamRoot := f.mkTeamSpace(t, owner.userID, "只读空间")
	// 造一个文件在团队空间根下
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1, $2, $3, 'shared.txt', false, 5, 1)`, teamID, teamRoot, owner.userID); err != nil {
		t.Fatalf("造文件失败: %v", err)
	}
	if err := (repo.SpaceRepo{}).AddMember(context.Background(), f.pool, teamID, f.userID, model.PermReader); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	res, err := f.svc.List(context.Background(), filesvc.ListInput{
		UserID: f.userID, SpaceID: teamID, ParentID: teamRoot,
	})
	if err != nil {
		t.Fatalf("reader 应可读: %v", err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("应有 1 个条目,实际 %d", len(res.Entries))
	}
}

// 非成员访问团队空间 → space_revoked(等价于被移出)
func TestListRejectsNonMemberOfTeamSpace(t *testing.T) {
	f := setup(t)
	owner := setup(t)
	teamID, teamRoot := f.mkTeamSpace(t, owner.userID, "他人团队")

	_, err := f.svc.List(context.Background(), filesvc.ListInput{
		UserID: f.userID, SpaceID: teamID, ParentID: teamRoot,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("非成员应 space_revoked,得到 %v", err)
	}
}

// parent 不属于该空间 → 403(防跨空间越权)
func TestListRejectsCrossSpaceParent(t *testing.T) {
	f := setup(t)
	other := setup(t)
	_, err := f.svc.List(context.Background(), filesvc.ListInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: other.rootID,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("跨空间 parent 应 403,得到 %v", err)
	}
}

// parent 是文件而非目录 → 400
func TestListRejectsNonDirParent(t *testing.T) {
	f := setup(t)
	fileID := f.mkFile(t, f.rootID, "plain.txt", 1, 1)
	_, err := f.svc.List(context.Background(), filesvc.ListInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: fileID,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("非目录 parent 应 400,得到 %v", err)
	}
}

// ---- 详情 ----

func TestGetFileMetadata(t *testing.T) {
	f := setup(t)
	id := f.mkFile(t, f.rootID, "detail.txt", 42, 5)

	v, err := f.svc.Get(context.Background(), f.userID, id)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if v.Size != 42 || v.Version != 5 {
		t.Fatalf("字段不对: %+v", v)
	}
	// 生成列 etag = lpad(to_hex(5),8,'0') || '-' || hash前8位;视图再统一加引号
	if !strings.HasPrefix(v.ETag, `"00000005-`) {
		t.Fatalf("ETag 应含 8 位十六进制版本号且带引号,实际 %q", v.ETag)
	}
	if !strings.HasSuffix(v.ETag, `"`) || len(v.ETag) != 1+8+1+8+1 {
		t.Fatalf("ETag 形状不对(应为 \"vvvvvvvv-hhhhhhhh\"): %q", v.ETag)
	}
}

func TestGetForeignFileRejected(t *testing.T) {
	f := setup(t)
	other := setup(t)
	id := other.mkFile(t, other.rootID, "not-yours.txt", 1, 1)

	_, err := f.svc.Get(context.Background(), f.userID, id)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("他人文件应 space_revoked,得到 %v", err)
	}
}

func TestGetMissingFileIs404(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Get(context.Background(), f.userID, "00000000-0000-7000-8000-000000000000")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("不存在的文件应 404,得到 %v", err)
	}
}

// ---- R-21 数据库侧:version / etag 生成列 ----

// etag 生成列**不含引号**(裸值),由 filesvc.QuoteETag 统一加引号。
func TestETagGeneratedColumnIsBare(t *testing.T) {
	f := setup(t)
	id := f.mkFile(t, f.rootID, "gen.txt", 1, 2)

	var raw string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT etag FROM files WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("读 etag 失败: %v", err)
	}
	if raw == "" {
		t.Fatal("etag 生成列不应为空")
	}
	if raw[0] == '"' {
		t.Fatalf("生成列必须**不含引号**(R-21),实际 %q", raw)
	}
	if raw[:9] != "00000002-" {
		t.Fatalf("etag 应以 8 位十六进制版本号开头,实际 %q", raw)
	}
}

// 版本自增 → 生成列自动变化(不需要应用层维护 etag)
func TestETagTracksVersion(t *testing.T) {
	f := setup(t)
	id := f.mkFile(t, f.rootID, "ver.txt", 1, 1)
	ctx := context.Background()

	var before string
	_ = f.pool.QueryRow(ctx, `SELECT etag FROM files WHERE id=$1`, id).Scan(&before)

	// 乐观锁自增版本
	// 注意:Go 不允许在 if 语句的初始化处对复合字面量的方法调用做多值接收,
	// 先取到变量再判断。
	touched, err := repo.FileRepo{}.TouchVersion(ctx, f.pool, id, 1)
	if err != nil {
		t.Fatalf("TouchVersion 失败: %v", err)
	}
	if touched.Version != 2 {
		t.Fatalf("自增后版本应为 2,实际 %d", touched.Version)
	}
	var after string
	_ = f.pool.QueryRow(ctx, `SELECT etag FROM files WHERE id=$1`, id).Scan(&after)

	if before == after {
		t.Fatalf("版本自增后 etag 必须变化,均为 %q", before)
	}
	if after[:9] != "00000002-" {
		t.Fatalf("自增后应是版本 2,实际 %q", after)
	}
}

// 乐观锁:版本不匹配 → ErrConflict(6.6 并发写矩阵)
func TestTouchVersionOptimisticLock(t *testing.T) {
	f := setup(t)
	id := f.mkFile(t, f.rootID, "opt.txt", 1, 1)
	ctx := context.Background()

	first, err := repo.FileRepo{}.TouchVersion(ctx, f.pool, id, 1)
	if err != nil {
		t.Fatalf("首次自增应成功: %v", err)
	}
	if first.Version != 2 {
		t.Fatalf("首次自增后版本应为 2,实际 %d", first.Version)
	}
	// 再用旧版本号 → 冲突
	_, err = repo.FileRepo{}.TouchVersion(ctx, f.pool, id, 1)
	if !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("旧版本号应 ErrConflict,实际 %v", err)
	}
}

// 唯一索引 (space_id, parent_id, lower(name)):a.txt 与 A.txt 不可共存(6.7 规则 6)
func TestUniqueIndexIsCaseInsensitive(t *testing.T) {
	f := setup(t)
	f.mkFile(t, f.rootID, "case.txt", 1, 1)

	_, err := f.pool.Exec(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1, $2, $3, 'CASE.TXT', false, 1, 1)`, f.spaceID, f.rootID, f.userID)
	if err == nil {
		t.Fatal("大小写不同的同名文件不应共存(唯一索引应拦住)")
	}
	if !repo.IsUniqueViolation(err) {
		t.Fatalf("应是唯一约束冲突,实际 %v", err)
	}
}

// 每空间恰有一行根(parent_id IS NULL,部分唯一索引)
func TestOnlyOneRootPerSpace(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(), `
INSERT INTO files (space_id, owner_id, name, is_dir, depth)
VALUES ($1, $2, '', true, 0)`, f.spaceID, f.userID)
	if err == nil {
		t.Fatal("同一空间不应有第二个根目录")
	}
}

// ---- 辅助 ----

// mkTeamSpace 建一个 team 空间 + 根目录(owner 是别人时用于测 reader/非成员)
func (f *fixture) mkTeamSpace(t *testing.T, ownerID, name string) (spaceID, rootID string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	var groupID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO groups (name, owner_id) VALUES ($1, $2) RETURNING id`,
		"fs_grp_"+suffix, ownerID).Scan(&groupID); err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO spaces (kind, group_id, owner_id, name, quota_bytes) VALUES ('team', $1, $2, $3, 0) RETURNING id`,
		groupID, ownerID, name).Scan(&spaceID); err != nil {
		t.Fatalf("建团队空间失败: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO files (space_id, owner_id, name, is_dir, depth) VALUES ($1, $2, '', true, 0) RETURNING id`,
		spaceID, ownerID).Scan(&rootID); err != nil {
		t.Fatalf("建团队根目录失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = f.pool.Exec(bg, `DELETE FROM files WHERE space_id = $1`, spaceID)
		_, _ = f.pool.Exec(bg, `DELETE FROM spaces WHERE id = $1`, spaceID)
		_, _ = f.pool.Exec(bg, `DELETE FROM groups WHERE id = $1`, groupID)
	})
	return spaceID, rootID
}

func names(es []filesvc.EntryView) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}
