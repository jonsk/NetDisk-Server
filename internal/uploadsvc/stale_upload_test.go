package uploadsvc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 「同名进行中上传」的**自愈**判定(BE-S4-02 的补口,2026-09-13 用户实测报告)。
//
// 真实现象:0 字节文件的旧客户端在定稿时调了秒传端点(需要 nonce)被 400 拒掉,而它**没有取消任务**;
// 于是那个 0 字节任务以 state='reserved' 占着"空间+父目录+名字"直到票过期(默认 24h)——
// 客户端之后每一次重试都只会拿到 409「同目录下已有一个正在上传的同名文件」,
// 用户看到的就是"这个文件怎么都同步不了"(实测:同一目录下三个空文件全卡住)。
//
// 修法的边界写在这里,四条断言逐条钉住:
//   - 只有**同一个用户**的空任务才会被回收(绝不动别人的上传);
//   - 只有**一个字节都没收到**的任务才算死掉(有进度的可能是用户暂停后续传,回收=扔掉已传的部分);
//   - 闲置要**超过**阈值(默认 30 分钟)才算死(慢网络下第一片可能要几分钟);
//   - 已过期的任务立即回收(票已失效,任务不可能再推进)。

func TestStaleEmptyUploadIsReapedOnCreate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now()
	f.svc.Now = func() time.Time { return now }

	// 先建一个任务,然后把它"放旧"31 分钟(空任务:一个字节都没收)
	first, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "空.bmp", DeclaredSize: 0,
	})
	if err != nil {
		t.Fatalf("建第一个任务失败: %v", err)
	}
	backdateUpload(t, f, first.UploadID, now.Add(-31*time.Minute))

	// 同名再建一次:必须成功(旧任务被回收),而不是 409
	second, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "空.bmp", DeclaredSize: 0,
	})
	if err != nil {
		t.Fatalf("闲置空任务应被回收并允许新建,实际 %v", err)
	}
	if second.UploadID == first.UploadID {
		t.Error("应当是一个**新**任务(旧任务被回收)")
	}
	if state := uploadState(t, f, first.UploadID); state == "reserved" {
		t.Errorf("旧任务应已释放,实际 state=%s", state)
	}
}

func TestFreshEmptyUploadStillBlocks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.svc.Now = func() time.Time { return time.Now() }

	// 刚建的空任务**不该**被回收:另一个客户端可能正要发第一片
	if _, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "刚建.bin", DeclaredSize: 0,
	}); err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	_, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "刚建.bin", DeclaredSize: 0,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("刚建的同名任务应仍然 409,实际 %v", err)
	}
}

func TestUploadWithProgressIsNotReaped(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now()
	f.svc.Now = func() time.Time { return now }

	first, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "断点.bin", DeclaredSize: 1024,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	// 收到过字节(用户暂停待续传)+ 闲置超过阈值
	if _, err := f.database.Pool.Exec(ctx,
		`UPDATE uploads SET uploaded_bytes = 512, created_at = $2 WHERE id = $1`,
		first.UploadID, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("造「有进度」现场失败: %v", err)
	}

	_, err = f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "断点.bin", DeclaredSize: 1024,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("已有进度的任务**不该**被回收(那是人家待续传的数据),实际 %v", err)
	}
	if state := uploadState(t, f, first.UploadID); state != "reserved" {
		t.Errorf("有进度的任务应保持 reserved,实际 %s", state)
	}
}

func TestExpiredUploadIsReaped(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now()
	f.svc.Now = func() time.Time { return now }

	first, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "过期.bin", DeclaredSize: 4,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if _, err := f.database.Pool.Exec(ctx,
		`UPDATE uploads SET expires_at = $2 WHERE id = $1`,
		first.UploadID, now.Add(-time.Minute)); err != nil {
		t.Fatalf("造过期现场失败: %v", err)
	}

	if _, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID, Name: "过期.bin", DeclaredSize: 4,
	}); err != nil {
		t.Fatalf("已过期的同名任务应被回收并允许新建,实际 %v", err)
	}
}

// ---------------------------------------------------------------- 工具

func backdateUpload(t *testing.T, f *fixture, uploadID string, at time.Time) {
	t.Helper()
	if _, err := f.database.Pool.Exec(context.Background(),
		`UPDATE uploads SET created_at = $2 WHERE id = $1`, uploadID, at); err != nil {
		t.Fatalf("回填 created_at 失败: %v", err)
	}
}

func uploadState(t *testing.T, f *fixture, uploadID string) string {
	t.Helper()
	var state string
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT state FROM uploads WHERE id = $1`, uploadID).Scan(&state); err != nil {
		t.Fatalf("读 uploads.state 失败: %v", err)
	}
	return state
}
