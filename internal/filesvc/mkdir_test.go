package filesvc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 建目录的 REST 入口(契约 `POST /api/v1/files/dirs`)。
//
// 为什么单独一组用例:目录创建原来只有 WebDAV MKCOL 一条路,而 MKCOL 要 Basic
// 账密;桌面端只持 Bearer 令牌,于是**建不了目录** —— 连带任何要放进子目录的文件
// 都传不上去。新入口必须有"严格新建、同名 409、要写权限、父必须同空间且是目录"
// 这几条自己的断言:少了任何一条,坏的那条路会以"看起来能用"的方式通过验收。

// ---- ① 正常新建 ----

func TestCreateDirCreatesRowWithDepthAndFeed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.svc.Feed = syncfeed.Writer{}

	v, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "文档",
	})
	if err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if !v.IsDir {
		t.Error("新建的条目 is_dir 应为 true")
	}
	if v.Name != "文档" {
		t.Errorf("名字应为 文档,实际 %q", v.Name)
	}
	if v.ParentID != f.rootID {
		t.Errorf("父应为同步根 %s,实际 %s", f.rootID, v.ParentID)
	}
	if v.SpaceID != f.spaceID {
		t.Errorf("空间应为 %s,实际 %s", f.spaceID, v.SpaceID)
	}
	// 行本身:depth 由**父的 depth+1** 算出(根是 0),不是写死的 1
	name, parent, _, depth := f.fileRow(t, v.ID)
	if name != "文档" || parent != f.rootID || depth != 1 {
		t.Errorf("行内容不符:name=%q parent=%q depth=%d", name, parent, depth)
	}
	// 变更流:其它客户端靠这条 created 行更新本地树
	if n := f.countFeedRows(t, v.ID, model.FeedCreated); n != 1 {
		t.Errorf("应恰好写 1 条 created 变更行,实际 %d", n)
	}
}

func TestCreateDirDepthFollowsParent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	parent := f.mkDirAt(t, f.rootID, "level1", 1)

	v, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: parent, Name: "level2",
	})
	if err != nil {
		t.Fatalf("建子目录失败: %v", err)
	}
	if _, _, _, depth := f.fileRow(t, v.ID); depth != 2 {
		t.Errorf("子目录 depth 应为 2(父 1 + 1),实际 %d", depth)
	}
}

// space_id / parent_id 都缺省 → 个人空间根目录(与列表接口同义)
func TestCreateDirDefaultsToPersonalSpaceRoot(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	v, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{UserID: f.userID, Name: "默认位置"})
	if err != nil {
		t.Fatalf("缺省参数建目录失败: %v", err)
	}
	if v.SpaceID != f.spaceID {
		t.Errorf("应落到个人空间 %s,实际 %s", f.spaceID, v.SpaceID)
	}
	if v.ParentID != f.rootID {
		t.Errorf("应落到空间根 %s,实际 %s", f.rootID, v.ParentID)
	}
}

// ---- ② 严格新建:同名一律 409,不返回已有条目 ----

func TestCreateDirDuplicateNameIsConflict(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "重复名",
	})
	if err != nil {
		t.Fatalf("第一次建目录失败: %v", err)
	}

	_, err = f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "重复名",
	})
	ae := asAPIErr(t, err)
	if ae.Status != 409 || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("重名应 409 name_conflict,实际 %d %s", ae.Status, ae.Code)
	}
	// reason 是客户端分支依据(提示"换个名字"而不是"刷新后重试")
	if ae.Details["reason"] != filesvc.ReasonNameConflict {
		t.Errorf("reason 应为 %s,实际 %v", filesvc.ReasonNameConflict, ae.Details["reason"])
	}
	// 冲突响应体要带**已有条目**的版本:客户端不必再查一次就能展示远端状态
	if v, ok := ae.Details["server_version"].(int64); !ok || v != first.Version {
		t.Errorf("server_version 应为 %d,实际 %v", first.Version, ae.Details["server_version"])
	}
}

// 唯一索引是 lower(name):大小写不同的同名也要拦
func TestCreateDirDuplicateIsCaseInsensitive(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "Docs",
	}); err != nil {
		t.Fatalf("第一次建目录失败: %v", err)
	}
	_, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "docs",
	})
	ae := asAPIErr(t, err)
	if ae.Code != apierr.CodeNameConflict {
		t.Fatalf("大小写不同的同名应 409 name_conflict,实际 %d %s", ae.Status, ae.Code)
	}
}

// 同名条目是**文件**时也必须拦:否则会出现"文件与目录同名"这种列表里两行同名、
// 客户端按名字定位永远只能找到其中一个的状态
func TestCreateDirRejectsExistingFileName(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.mkFile(t, f.rootID, "same.txt", 3, 1)

	_, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "same.txt",
	})
	ae := asAPIErr(t, err)
	if ae.Code != apierr.CodeNameConflict {
		t.Fatalf("同名文件已存在应 409 name_conflict,实际 %d %s", ae.Status, ae.Code)
	}
}

// ---- ③ 参数与权限 ----

func TestCreateDirRejectsInvalidName(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// "/" 是半角禁用字符(6.7:服务端建议换成全角"／")
	_, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "a/b",
	})
	ae := asAPIErr(t, err)
	if ae.Status != 400 || ae.Code != apierr.CodeInvalidName {
		t.Fatalf("含 / 的名字应 400 invalid_name,实际 %d %s", ae.Status, ae.Code)
	}
	// 空名字(归一后为空)单独一档:400 invalid_argument
	_, err = f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "   ",
	})
	if ae := asAPIErr(t, err); ae.Status != 400 || ae.Code != apierr.CodeInvalidArgument {
		t.Fatalf("空名字应 400 invalid_argument,实际 %d %s", ae.Status, ae.Code)
	}
}

// 父必须是目录
func TestCreateDirRejectsNonDirParent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	fileID := f.mkFile(t, f.rootID, "not-a-dir.txt", 3, 1)

	_, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: fileID, Name: "子目录",
	})
	ae := asAPIErr(t, err)
	if ae.Status != 400 || ae.Code != apierr.CodeInvalidArgument {
		t.Fatalf("父是文件应 400 invalid_argument,实际 %d %s", ae.Status, ae.Code)
	}
}

// 父目录属于别的空间 → 403:空间即权限边界,不能靠换 parent_id 越权
func TestCreateDirRejectsCrossSpaceParent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	other := setup(t)

	_, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: other.rootID, Name: "越权",
	})
	ae := asAPIErr(t, err)
	if ae.Status != 403 {
		t.Fatalf("跨空间父目录应 403,实际 %d %s", ae.Status, ae.Code)
	}
}

// 只读成员不能建目录(4.3)。这条单独断言:少了它,"只读"会在**服务端**被绕过,
// 而客户端侧的只读展示看起来完全正常。
func TestCreateDirRequiresWritePermission(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	owner := setup(t)
	teamID, teamRoot := f.mkTeamSpace(t, owner.userID, "只读建目录空间")
	if err := (repo.SpaceRepo{}).AddMember(ctx, f.pool, teamID, f.userID, model.PermReader); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}

	_, err := f.svc.CreateDir(ctx, filesvc.CreateDirInput{
		UserID: f.userID, SpaceID: teamID, ParentID: teamRoot, Name: "只读不该建出来",
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("只读成员建目录应 403,实际 %v", err)
	}
	// 反向确认:库里确实没有这一行(403 不是"建了才发现"的响应)
	if _, err := f.svc.List(ctx, filesvc.ListInput{
		UserID: f.userID, SpaceID: teamID, ParentID: teamRoot,
	}); err != nil {
		t.Fatalf("列出团队空间失败: %v", err)
	}
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE space_id=$1 AND name='只读不该建出来'`, teamID).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Errorf("被拒的建目录不应留下行,实际 %d 行", n)
	}
}
