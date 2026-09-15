package uploadsvc_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 本文件是 uploadsvc 的**真实数据库**集成测试。
// 未设置 NETDISK_TEST_DSN 时全部 skip(与 authsvc 的约定一致,E-03)。

type fixture struct {
	svc      *uploadsvc.Service
	database *db.DB
	userID   string
	spaceID  string
	rootID   string
}

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

	suffix := time.Now().Format("150405.000000")
	users := repo.UserRepo{}
	var created *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		u, err := users.Create(ctx, tx, repo.CreateInput{
			Username:    "up_" + suffix,
			Email:       "up_" + suffix + "@example.com",
			DisplayName: "上传测试用户",
			Role:        model.RoleUser,
		})
		if err != nil {
			return err
		}
		created = u
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}

	spaces := repo.SpaceRepo{}
	sp, err := spaces.PersonalOf(ctx, database.Pool, created.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	files := repo.FileRepo{}
	root, err := files.GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}

	svc := &uploadsvc.Service{
		Spaces:  spaces,
		Files:   files,
		Uploads: repo.UploadRepo{},
		DB:      db.AsQuerier(database),
		Name:    namepolicy.Default(),
	}

	t.Cleanup(func() {
		// 清理顺序:uploads → 空间内文件引用的对象 → files → spaces → users。
		//
		// 之前这里只删 uploads 与 users —— 而 `DELETE FROM users` 会被
		// files.owner_id 的 ON DELETE RESTRICT 挡下(错误被 `_, _ =` 吞掉),
		// 于是 files 与 file_objects 逐轮累积:第二次运行同一用例时
		// ref_count 变成 4 而不是 2(实测),失败现象完全指向错误的地方。
		//
		// 与其它包一致:**没有任何无条件 DELETE**,每条都带明确限定。
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM uploads WHERE user_id = $1`, created.ID)
		_, _ = database.Pool.Exec(context.Background(), `
DELETE FROM file_objects WHERE hash_sha256 IN (
  SELECT DISTINCT hash_sha256 FROM files
   WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL)`, created.ID)
		_, _ = database.Pool.Exec(context.Background(),
			`DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`, created.ID)
		_, _ = database.Pool.Exec(context.Background(),
			`DELETE FROM spaces WHERE owner_id = $1`, created.ID)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, created.ID)
	})

	return &fixture{svc: svc, database: database, userID: created.ID, spaceID: sp.ID, rootID: root.ID}
}

// used 读回空间已用字节(直接查库,不信服务端内存态)。
func (f *fixture) used(t *testing.T) int64 {
	t.Helper()
	var v int64
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT used_bytes FROM spaces WHERE id = $1`, f.spaceID).Scan(&v); err != nil {
		t.Fatalf("读 used_bytes 失败: %v", err)
	}
	return v
}

// setQuota 调整空间配额(0 = 不限制)。
func (f *fixture) setQuota(t *testing.T, quota int64) {
	t.Helper()
	if _, err := f.database.Pool.Exec(context.Background(),
		`UPDATE spaces SET quota_bytes = $2, used_bytes = 0 WHERE id = $1`, f.spaceID, quota); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
}

// create 建一个上传任务,失败即终止用例。
func (f *fixture) create(t *testing.T, name string, size int64) *uploadsvc.CreateResult {
	t.Helper()
	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: name, DeclaredSize: size,
	})
	if err != nil {
		t.Fatalf("Create(%s) 失败: %v", name, err)
	}
	return res
}

// ---- 基本路径 ----

func TestCreateReservesQuota(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 1000)

	res := f.create(t, "报告.pdf", 400)

	if res.UploadID == "" || res.Ticket == "" {
		t.Fatalf("应返回 upload_id 与 ticket,得到 %+v", res)
	}
	if got := f.used(t); got != 400 {
		t.Fatalf("创建后应预留 400 字节,实际 %d", got)
	}
	if res.QuotaAfter != 600 {
		t.Fatalf("剩余额度应为 600,实际 %d", res.QuotaAfter)
	}
	if !res.ExpiresAt.After(time.Now()) {
		t.Fatalf("过期时间应在未来,实际 %v", res.ExpiresAt)
	}
}

// 二次预留叠加:两次各 400,总占用 800 —— 证明预留是累加的,不是覆盖。
func TestReserveAccumulates(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 1000)
	f.create(t, "a.bin", 400)
	f.create(t, "b.bin", 400)

	if got := f.used(t); got != 800 {
		t.Fatalf("两次预留应累计 800,实际 %d", got)
	}
}

// 4.3 的核心断言:**超额要在创建时就被拒**,而不是传完再拒。
func TestCreateRejectsWhenQuotaExceeded(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 1000)
	f.create(t, "first.bin", 900)

	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "second.bin", DeclaredSize: 200,
	})
	if err == nil {
		t.Fatal("额度不足时 Create 应失败")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("应返回 *apierr.Error,得到 %T", err)
	}
	if ae.Status != 507 {
		t.Fatalf("额度不足应返回 507 Insufficient Storage,实际 %d(%s)", ae.Status, ae.Code)
	}
	// 失败不得留下任何预留痕迹
	if got := f.used(t); got != 900 {
		t.Fatalf("失败的创建不应改变 used_bytes,期望 900,实际 %d", got)
	}
	var n int
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM uploads WHERE user_id = $1`, f.userID).Scan(&n); err != nil {
		t.Fatalf("统计上传任务失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("失败请求不应落任务行,期望 1 行,实际 %d", n)
	}
}

// 边界:恰好用完额度必须成功(= 而不是 <)。
func TestCreateAllowsExactFit(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 1000)
	res := f.create(t, "exact.bin", 1000)
	if res.QuotaAfter != 0 {
		t.Fatalf("恰好用满时剩余应为 0,实际 %d", res.QuotaAfter)
	}
}

// quota_bytes = 0 表示不限制:再大也放行,且 QuotaAfter 用 -1 表达"不限"。
func TestCreateUnlimitedQuota(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	res := f.create(t, "big.bin", 1<<40)
	if res.QuotaAfter != -1 {
		t.Fatalf("不限额度的 QuotaAfter 应为 -1,实际 %d", res.QuotaAfter)
	}
}

// ---- 命名与冲突(6.7 服务器是唯一裁判)----

func TestCreateRejectsInvalidName(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "a/b.txt", DeclaredSize: 1,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("非法名应返回 *apierr.Error,得到 %v", err)
	}
	if ae.Code != apierr.CodeInvalidName {
		t.Fatalf("应为 invalid_name,实际 %s", ae.Code)
	}
	// 6.7:必须给出规则号与建议,客户端才能引导用户改正
	if ae.Details["rule"] == nil {
		t.Fatalf("非法名错误应带 rule 明细,实际 %+v", ae.Details)
	}
	if ae.Details["suggestion"] == nil {
		t.Fatalf("禁用字符应给出全角建议名,实际 %+v", ae.Details)
	}
	// 任何校验失败都不应占额度
	if got := f.used(t); got != 0 {
		t.Fatalf("非法名不应预留额度,实际 %d", got)
	}
}

// NFD 输入必须被 NFC 归一后再比较:é 的两种写法应视为同名。
func TestCreateNormalizesAndDetectsConflict(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	f.create(t, "cafe\u0301.txt", 10) // NFD: e + 组合尖音符

	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "caf\u00e9.txt", DeclaredSize: 10, // NFC
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("NFC/NFD 应视为同名并返回 name_conflict,得到 %v", err)
	}
}

// 同名冲突有两个来源:已存在的 files 行,以及**进行中的同名上传**。
// 这里覆盖第二个来源 —— 否则两个客户端同时传同名文件都能建任务,第二个会白传到定稿才失败。
func TestCreateRejectsSameNameInFlightUpload(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	first := f.create(t, "dup.txt", 10)

	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "dup.txt", DeclaredSize: 10,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("进行中的同名上传应返回 name_conflict,得到 %v", err)
	}
	if ae.Status != 409 {
		t.Fatalf("同名冲突应为 409,实际 %d", ae.Status)
	}
	if got := f.used(t); got != 10 {
		t.Fatalf("冲突请求不应二次预留,期望 10,实际 %d", got)
	}

	// 取消后名字必须立刻可以复用,否则用户会被自己刚取消的任务卡住
	if err := f.svc.Cancel(context.Background(), first.UploadID, f.userID); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	if _, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "dup.txt", DeclaredSize: 10,
	}); err != nil {
		t.Fatalf("取消后同名上传应可重建: %v", err)
	}
}

// 已存在的 files 行(定稿产物)同样占名 —— 覆盖第一个来源。
func TestCreateRejectsExistingFileName(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	if _, err := f.database.Pool.Exec(context.Background(),
		`INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
		 VALUES ($1, $2, $3, $4, false, 5, 1)`,
		f.spaceID, f.rootID, f.userID, "exists.txt"); err != nil {
		t.Fatalf("造已存在文件失败: %v", err)
	}
	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "exists.txt", DeclaredSize: 10,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("同名文件应返回 name_conflict,得到 %v", err)
	}
}

// 覆盖意图必须"显式声明 + 落库":默认同名 409;只有显式 allow_overwrite 才放行,
// 且意图要写进任务行 —— 任务可能隔很久才传完(甚至跨进程重启),定稿那一刻只能靠
// 库里这份意图判断该不该覆盖。丢了它,"改本地文件传回去"会在**最后一片**才 409,
// 前面的字节全白传(实测客户端就是这样:本地改了文件、传了几 MB 后在定稿时被拒)。
func TestCreateAllowsOverwriteOnlyWhenExplicit(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	if _, err := f.database.Pool.Exec(context.Background(),
		`INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
		 VALUES ($1, $2, $3, $4, false, 5, 1)`,
		f.spaceID, f.rootID, f.userID, "over.txt"); err != nil {
		t.Fatalf("造已存在文件失败: %v", err)
	}

	// ① 不声明意图 → 仍然 409(6.7:绝不静默覆盖)
	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "over.txt", DeclaredSize: 10,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("未声明覆盖时必须 409 name_conflict(绝不静默覆盖),得到 %v", err)
	}

	// ② 显式声明覆盖 → 放行
	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "over.txt",
		DeclaredSize: 10, AllowOverwrite: true,
	})
	if err != nil {
		t.Fatalf("显式 allow_overwrite 应放行同名建任务,得到 %v", err)
	}
	if res == nil || res.UploadID == "" {
		t.Fatal("应返回 upload_id")
	}

	// ③ 意图必须落库(定稿在另一时刻/另一进程读它)
	var stored bool
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT allow_overwrite FROM uploads WHERE id = $1`, res.UploadID).Scan(&stored); err != nil {
		t.Fatalf("查任务行失败: %v", err)
	}
	if !stored {
		t.Fatal("allow_overwrite 必须落库为 true,否则定稿时会退化成 409")
	}

	// ④ 但"进行中的同名任务"永远挡住第二个建任务(第 4b 条不受 allow_overwrite 影响):
	//    否则两个同名任务会在定稿时互相覆盖,用户看到的是随机结果。
	_, err = f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "over.txt",
		DeclaredSize: 10, AllowOverwrite: true,
	})
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("已有同名任务在进行中时必须 409(即使声明了覆盖),得到 %v", err)
	}
}

// ---- 权限与空间状态 ----
func TestCreateRejectsForeignSpace(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)

	// 另一个用户 + 他的个人空间
	other := setup(t)
	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: other.spaceID, ParentID: other.rootID,
		Name: "steal.txt", DeclaredSize: 10,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("跨空间上传应失败,得到 %v", err)
	}
	if ae.Status != 403 {
		t.Fatalf("别人的个人空间应 403,实际 %d(%s)", ae.Status, ae.Code)
	}
	if ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("应使用 space_revoked(客户端据此不删本地),实际 %s", ae.Code)
	}
}

func TestCreateRejectsFrozenSpace(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	if _, err := f.database.Pool.Exec(context.Background(),
		`UPDATE spaces SET frozen = true WHERE id = $1`, f.spaceID); err != nil {
		t.Fatalf("冻结空间失败: %v", err)
	}
	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "x.txt", DeclaredSize: 10,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("冻结空间应 space_revoked,得到 %v", err)
	}
}

func TestCreateRejectsReaderPermission(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)

	// 关键:**空间 owner 必须是别人** —— owner 会被 GetMembership 提升为 manager,
	// 若把 owner 设成当前用户就永远测不出 reader 分支。
	owner := setup(t)

	// 建一个 team 空间,把当前用户加成 reader(team 空间必须挂 group,见 spaces_kind_group_chk)
	var groupID string
	if err := f.database.Pool.QueryRow(context.Background(),
		`INSERT INTO groups (name, owner_id) VALUES ($1, $2) RETURNING id`,
		"只读测试组", owner.userID).Scan(&groupID); err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	var teamID string
	if err := f.database.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (kind, group_id, owner_id, name, quota_bytes) VALUES ('team', $1, $2, $3, 0) RETURNING id`,
		groupID, owner.userID, "只读团队空间").Scan(&teamID); err != nil {
		t.Fatalf("建团队空间失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.database.Pool.Exec(context.Background(), `DELETE FROM spaces WHERE id = $1`, teamID)
		_, _ = f.database.Pool.Exec(context.Background(), `DELETE FROM spaces WHERE group_id = $1`, groupID)
		_, _ = f.database.Pool.Exec(context.Background(), `DELETE FROM groups WHERE id = $1`, groupID)
	})
	var teamRoot string
	if err := f.database.Pool.QueryRow(context.Background(),
		`INSERT INTO files (space_id, owner_id, name, is_dir, depth) VALUES ($1, $2, '', true, 0) RETURNING id`,
		teamID, owner.userID).Scan(&teamRoot); err != nil {
		t.Fatalf("建团队根目录失败: %v", err)
	}
	if err := (repo.SpaceRepo{}).AddMember(context.Background(), f.database.Pool, teamID, f.userID, model.PermReader); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}

	// 先证明权限判定确实看到的是 reader(否则下面的断言可能是"碰巧"过的)
	m, err := (repo.SpaceRepo{}).GetMembership(context.Background(), f.database.Pool, teamID, f.userID)
	if err != nil {
		t.Fatalf("GetMembership 失败: %v", err)
	}
	if m == nil || m.Permission != model.PermReader {
		t.Fatalf("前置条件不成立:期望 reader,实际 %+v", m)
	}

	_, err = f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: teamID, ParentID: teamRoot, Name: "nope.txt", DeclaredSize: 10,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("只读权限上传应 403,得到 %v", err)
	}
	if !strings.Contains(ae.Message, "只读") {
		t.Fatalf("403 文案应说明是只读权限,实际 %q", ae.Message)
	}
}

func TestCreateRejectsWrongParent(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	res := f.create(t, "afile.bin", 10) // 得到一个任务(用于后面的目标校验)
	_ = res

	// 直接拿 upload 当目录不行;这里用"不存在的目录"
	_, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: "00000000-0000-7000-8000-000000000000",
		Name: "x.txt", DeclaredSize: 10,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("不存在的父目录应 404,得到 %v", err)
	}
}

// ---- ticket 校验 ----

func TestAuthorizeAcceptsValidTicket(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	res := f.create(t, "auth.bin", 100)

	up, err := f.svc.Authorize(context.Background(), res.UploadID, f.userID, res.Ticket)
	if err != nil {
		t.Fatalf("有效 ticket 应通过: %v", err)
	}
	if up.Name != "auth.bin" || up.DeclaredSize != 100 {
		t.Fatalf("返回的任务字段不对: %+v", up)
	}
	// 明文绝不落入数据库
	if strings.Contains(up.TicketHash, res.Ticket) {
		t.Fatal("upload 行里出现了明文 ticket")
	}
	if len(up.TicketHash) != 64 {
		t.Fatalf("ticket_hash 应为 sha256 hex(64 字符),实际 %d", len(up.TicketHash))
	}
}

func TestAuthorizeRejectsWrongTicket(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	res := f.create(t, "auth2.bin", 100)

	_, err := f.svc.Authorize(context.Background(), res.UploadID, f.userID, "not-the-ticket")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("错误 ticket 应 401,得到 %v", err)
	}
}

// 别人的 ticket 不能操作我的任务,且**不能泄露任务是否存在**(统一 404)。
func TestAuthorizeHidesForeignUpload(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	res := f.create(t, "mine.bin", 100)

	other := setup(t)
	_, err := f.svc.Authorize(context.Background(), res.UploadID, other.userID, res.Ticket)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("他人任务应统一 404(不泄露存在性),得到 %v", err)
	}
}

// 过期判定用"创建时刻 + TTL"与当前时刻比较,所以时钟要回拨到 TTL 之前。
func TestAuthorizeRejectsExpiredTicket(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	f.svc.Now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
	res := f.create(t, "old.bin", 100)

	f.svc.Now = nil // 回到真实时钟
	_, err := f.svc.Authorize(context.Background(), res.UploadID, f.userID, res.Ticket)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeUploadGone {
		t.Fatalf("过期 ticket 应 upload_gone,得到 %v", err)
	}
}

// ---- 取消与回收 ----

func TestCancelReleasesImmediately(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 1000)
	res := f.create(t, "cancel.bin", 700)
	if got := f.used(t); got != 700 {
		t.Fatalf("预留应为 700,实际 %d", got)
	}

	if err := f.svc.Cancel(context.Background(), res.UploadID, f.userID); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	if got := f.used(t); got != 0 {
		t.Fatalf("取消后应即时释放额度(6.3),实际 %d", got)
	}
	// 取消必须幂等:重复取消不报错,也不再扣额度
	if err := f.svc.Cancel(context.Background(), res.UploadID, f.userID); err != nil {
		t.Fatalf("重复 Cancel 应幂等成功: %v", err)
	}
	if got := f.used(t); got != 0 {
		t.Fatalf("重复取消不应把额度扣成负数,实际 %d", got)
	}
}

// 取消后 ticket 失效(任务已非 reserved)。
func TestCancelInvalidatesTicket(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	res := f.create(t, "gone.bin", 10)
	if err := f.svc.Cancel(context.Background(), res.UploadID, f.userID); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	_, err := f.svc.Authorize(context.Background(), res.UploadID, f.userID, res.Ticket)
	if err == nil {
		t.Fatal("已取消的任务不应再通过 ticket 校验")
	}
}

func TestCancelForeignUploadDenied(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 1000)
	res := f.create(t, "keep.bin", 500)

	other := setup(t)
	err := f.svc.Cancel(context.Background(), res.UploadID, other.userID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("取消他人任务应 404,得到 %v", err)
	}
	if got := f.used(t); got != 500 {
		t.Fatalf("越权取消不得释放额度,期望 500,实际 %d", got)
	}
	// 状态必须原封不动:越权者既不能取消,也不能把任务搞成 released
	var state string
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT state FROM uploads WHERE id = $1`, res.UploadID).Scan(&state); err != nil {
		t.Fatalf("读任务状态失败: %v", err)
	}
	if state != model.UploadReserved {
		t.Fatalf("越权取消后状态应仍为 reserved,实际 %s", state)
	}
	// 原主仍然可以正常使用该 ticket
	if _, err := f.svc.Authorize(context.Background(), res.UploadID, f.userID, res.Ticket); err != nil {
		t.Fatalf("越权尝试不应影响原主的 ticket: %v", err)
	}
}

// ReleaseExpired 是兜底路径:过期任务被回收,额度回到空间。
func TestReleaseExpiredReclaimsQuota(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 1000)
	// 时钟回拨 48h → expires_at(= 回拨时刻 + 24h)落在真实 now 之前,即已过期
	f.svc.Now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
	created := f.create(t, "stale.bin", 600)
	upID := created.UploadID

	f.svc.Now = nil
	n, err := f.svc.ReleaseExpired(context.Background(), 100)
	if err != nil {
		t.Fatalf("ReleaseExpired 失败: %v", err)
	}
	// **不断言 n 恰好等于 1**:ReleaseExpired 是**全库**清扫(生产语义如此:
	// 后台任务要扫掉所有人的过期预留),而集成测试库里同时可能有别的包/别的用例
	// 留下的过期行 —— Go 的测试包之间是并行执行的,断言"恰好 1 个"会变成
	// "单跑必过、全量跑随机挂"的假失败。
	//
	// 正确的断言方式是**只看本用例那一行**:它必须被置为 released,
	// 且它占的额度必须被退回。这两条与"库里有几条别人的过期行"无关。
	if n < 1 {
		t.Fatalf("至少应回收本用例那 1 个过期任务,实际 %d", n)
	}
	var state string
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT state FROM uploads WHERE id = $1`, upID).Scan(&state); err != nil {
		t.Fatalf("读任务状态失败: %v", err)
	}
	if state != model.UploadReleased {
		t.Fatalf("本用例的过期任务应被置为 released,实际 %s", state)
	}
	if got := f.used(t); got != 0 {
		t.Fatalf("回收后额度应归零,实际 %d", got)
	}
}

// ---- 进度推进(TUS Upload-Offset 的真相来源)----

func TestSetUploadedIsMonotonic(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	res := f.create(t, "chunk.bin", 1000)

	uploads := repo.UploadRepo{}
	up, err := uploads.SetUploaded(context.Background(), f.database.Pool, res.UploadID, 300)
	if err != nil {
		t.Fatalf("推进进度失败: %v", err)
	}
	if up.UploadedBytes != 300 {
		t.Fatalf("进度应为 300,实际 %d", up.UploadedBytes)
	}
	// 重复/乱序分片不得把进度写回去(否则 HEAD 会给错 offset → 数据损坏)
	if _, err := uploads.SetUploaded(context.Background(), f.database.Pool, res.UploadID, 200); !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("回退进度应返回 ErrConflict,实际 %v", err)
	}
	again, err := uploads.GetByID(context.Background(), f.database.Pool, res.UploadID)
	if err != nil {
		t.Fatalf("读回任务失败: %v", err)
	}
	if again.UploadedBytes != 300 {
		t.Fatalf("进度应保持 300,实际 %d", again.UploadedBytes)
	}
}

// 声明大小之上不接受数据(库层 CHECK 兜底)。
func TestSetUploadedRejectsBeyondDeclared(t *testing.T) {
	f := setup(t)
	f.setQuota(t, 0)
	res := f.create(t, "small.bin", 100)
	_, err := repo.UploadRepo{}.SetUploaded(context.Background(), f.database.Pool, res.UploadID, 101)
	if err == nil {
		t.Fatal("超过声明大小应被数据库约束拒绝")
	}
}
