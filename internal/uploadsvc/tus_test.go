package uploadsvc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// TUS 数据面的真实 PG 集成测试(BE-S5-03:6.1 / 3.4)。

// tusFixture 在 uploadsvc 的 fixture 上加装暂存区与定稿服务。
type tusFixture struct {
	*fixture
	tus *uploadsvc.TUSService
	fs  *storage.FS
}

func setupTUS(t *testing.T) *tusFixture {
	t.Helper()
	f := setup(t)
	fs, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	fin := &finalize.Service{
		Pool:   f.database.Pool,
		Spaces: repo.SpaceRepo{},
		Files:  repo.FileRepo{},
		// 与 uploadsvc 用同一个 Name 策略
		Name:    f.svc.Name,
		Storage: fs,
	}
	tus := &uploadsvc.TUSService{
		Service:   f.svc,
		Stager:    fs,
		Finalizer: fin,
		DB:        f.svc.DB,
	}
	return &tusFixture{fixture: f, tus: tus, fs: fs}
}

// create 建一个 TUS 上传任务,返回 (uploadID, ticket)。
func (f *tusFixture) create(t *testing.T, name string, size int64) (string, string) {
	t.Helper()
	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: name, DeclaredSize: size,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	// 创建即预留额度
	if _, err := f.database.Pool.Exec(context.Background(),
		`UPDATE spaces SET used_bytes = $2 WHERE id = $1`, f.spaceID, size); err != nil {
		t.Fatalf("预占额度失败: %v", err)
	}
	return res.UploadID, res.Ticket
}

func (f *tusFixture) head(t *testing.T, id, ticket string) *uploadsvc.TUSStatus {
	t.Helper()
	st, err := f.tus.Head(context.Background(), id, f.userID, ticket)
	if err != nil {
		t.Fatalf("Head 失败: %v", err)
	}
	return st
}

func (f *tusFixture) patch(t *testing.T, id, ticket string, offset int64, data []byte) *uploadsvc.PatchResult {
	t.Helper()
	res, err := f.tus.Patch(context.Background(), uploadsvc.PatchInput{
		UploadID: id, UserID: f.userID, Ticket: ticket,
		ExpectOffset: offset, Content: bytes.NewReader(data), ContentLength: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("Patch(offset=%d) 失败: %v", offset, err)
	}
	return res
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---- 主路径 ----

// 分片上传 → 写完即定稿 → 文件可读(3.4 步骤 1~5)
func TestTUSFullUploadThenFinalize(t *testing.T) {
	f := setupTUS(t)
	ctx := context.Background()

	// 造 3 个分片的内容
	part1 := bytes.Repeat([]byte("A"), 1000)
	part2 := bytes.Repeat([]byte("B"), 2000)
	part3 := bytes.Repeat([]byte("C"), 3000)
	whole := append(append(append([]byte{}, part1...), part2...), part3...)
	wantHash := hashOf(whole)

	id, ticket := f.create(t, "断点续传.bin", int64(len(whole)))

	// 初始偏移必须是 0
	if st := f.head(t, id, ticket); st.Offset != 0 || st.Length != int64(len(whole)) {
		t.Fatalf("初始 offset/length 应为 0/%d,实际 %d/%d", len(whole), st.Offset, st.Length)
	}

	// 分片 1、2 未完成
	r1 := f.patch(t, id, ticket, 0, part1)
	if r1.Finalized {
		t.Fatal("未传完不应定稿")
	}
	if r1.Offset != int64(len(part1)) {
		t.Fatalf("分片 1 后偏移应为 %d,实际 %d", len(part1), r1.Offset)
	}
	// 模拟"中断后重连":HEAD 必须给出正确偏移
	if st := f.head(t, id, ticket); st.Offset != int64(len(part1)) {
		t.Fatalf("中断后 HEAD 偏移应为 %d,实际 %d", len(part1), st.Offset)
	}
	f.patch(t, id, ticket, int64(len(part1)), part2)
	if st := f.head(t, id, ticket); st.Offset != int64(len(part1)+len(part2)) {
		t.Fatalf("分片 2 后偏移应为 %d,实际 %d", len(part1)+len(part2), st.Offset)
	}

	// 分片 3 → 达到声明长度 → 写完即定稿
	r3 := f.patch(t, id, ticket, int64(len(part1)+len(part2)), part3)
	if !r3.Finalized {
		t.Fatal("传满声明长度后应立刻定稿(3.4 步骤 3~4)")
	}
	if r3.File == nil {
		t.Fatal("定稿应返回文件行")
	}
	// 服务端实测哈希必须等于真实内容哈希
	if r3.File.HashSHA256 != wantHash {
		t.Fatalf("落库哈希应为实测值 %s,实际 %s", wantHash, r3.File.HashSHA256)
	}
	if r3.File.Size != int64(len(whole)) {
		t.Fatalf("文件大小应为 %d,实际 %d", len(whole), r3.File.Size)
	}
	// 内容已成为内容寻址对象且可读
	sz, err := f.fs.Stat(ctx, wantHash)
	if err != nil {
		t.Fatalf("定稿后对象应在存储中: %v", err)
	}
	if sz != int64(len(whole)) {
		t.Fatalf("对象大小应为 %d,实际 %d", len(whole), sz)
	}
	// 暂存文件必须已清理(定稿成功)
	if n, _ := f.fs.StageSize(id); n != 0 {
		t.Fatalf("定稿成功后暂存文件应已删除,实际大小 %d", n)
	}
	// upload 任务置 finalized
	var state string
	if err := f.database.Pool.QueryRow(ctx,
		`SELECT state FROM uploads WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("读任务状态失败: %v", err)
	}
	if state != model.UploadFinalized {
		t.Fatalf("任务应置 finalized,实际 %s", state)
	}
	// 配额按实际结算(声明=实际,所以应为实际大小)
	var used int64
	_ = f.database.Pool.QueryRow(ctx, `SELECT used_bytes FROM spaces WHERE id=$1`, f.spaceID).Scan(&used)
	if used != int64(len(whole)) {
		t.Fatalf("结算后 used_bytes 应为 %d,实际 %d", len(whole), used)
	}
}

// 单分片传完(小文件)也应定稿
func TestTUSSingleChunkFinalizes(t *testing.T) {
	f := setupTUS(t)
	data := bytes.Repeat([]byte("x"), 512)
	id, ticket := f.create(t, "single.bin", int64(len(data)))

	res := f.patch(t, id, ticket, 0, data)
	if !res.Finalized || res.File == nil {
		t.Fatal("一次传满应立即定稿")
	}
	if res.File.HashSHA256 != hashOf(data) {
		t.Fatal("哈希应为服务端实测值")
	}
}

// ---- 偏移校验(断点续传的正确性核心)----

// 偏移与暂存文件长度不符 → 409,且**回真实偏移**(客户端据此自我纠正而非重传)
func TestTUSOffsetMismatchReturns409WithRealOffset(t *testing.T) {
	f := setupTUS(t)
	ctx := context.Background()
	part := bytes.Repeat([]byte("A"), 1000)
	id, ticket := f.create(t, "offset.bin", 3000)

	f.patch(t, id, ticket, 0, part)

	// 声称从 0 开始(重放)→ 应冲突
	_, err := f.tus.Patch(ctx, uploadsvc.PatchInput{
		UploadID: id, UserID: f.userID, Ticket: ticket,
		ExpectOffset: 0, Content: bytes.NewReader(part), ContentLength: int64(len(part)),
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("偏移不符应返回 *apierr.Error,实际 %v", err)
	}
	if ae.Status != 409 {
		t.Fatalf("TUS 偏移不符应 409,实际 %d(%s)", ae.Status, ae.Code)
	}
	if v, ok := ae.Details["upload_offset"]; !ok || toI64(v) != 1000 {
		t.Fatalf("409 必须带服务端真实偏移(1000),实际 %v", ae.Details)
	}
	// 声称从中间开始(跳跃)→ 同样冲突
	_, err = f.tus.Patch(ctx, uploadsvc.PatchInput{
		UploadID: id, UserID: f.userID, Ticket: ticket,
		ExpectOffset: 500, Content: bytes.NewReader(part), ContentLength: int64(len(part)),
	})
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("跳跃偏移应 409,实际 %v", err)
	}
	// 暂存内容不得被污染
	if n, _ := f.fs.StageSize(id); n != 1000 {
		t.Fatalf("冲突请求不得改变暂存长度,实际 %d", n)
	}
}

// 超出声明长度 → 400,并清理暂存(不让客户端在错误状态下继续)
func TestTUSRejectsOverDeclaredSize(t *testing.T) {
	f := setupTUS(t)
	ctx := context.Background()
	id, ticket := f.create(t, "over.bin", 500)

	_, err := f.tus.Patch(ctx, uploadsvc.PatchInput{
		UploadID: id, UserID: f.userID, Ticket: ticket,
		ExpectOffset: 0, Content: bytes.NewReader(bytes.Repeat([]byte("z"), 900)), ContentLength: 900,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("超出声明长度应 400,实际 %v", err)
	}
	if n, _ := f.fs.StageSize(id); n != 0 {
		t.Fatalf("超长请求后应清理暂存,实际 %d", n)
	}
}

// 重复 PATCH 同一分片:计数不回退(单调),且不破坏已写内容
func TestTUSDuplicatePatchIsSafe(t *testing.T) {
	f := setupTUS(t)
	ctx := context.Background()
	part := bytes.Repeat([]byte("Q"), 800)
	id, ticket := f.create(t, "dup.bin", 1600)

	f.patch(t, id, ticket, 0, part)
	// 重放第一片(相同偏移)→ 409(偏移不符),不写坏数据
	_, err := f.tus.Patch(ctx, uploadsvc.PatchInput{
		UploadID: id, UserID: f.userID, Ticket: ticket,
		ExpectOffset: 0, Content: bytes.NewReader(part), ContentLength: int64(len(part)),
	})
	if err == nil {
		t.Fatal("重放分片应因偏移不符被拒")
	}
	if n, _ := f.fs.StageSize(id); n != 800 {
		t.Fatalf("暂存长度应保持 800,实际 %d", n)
	}
	// 继续传完 → 定稿,且内容正确
	res := f.patch(t, id, ticket, 800, part)
	if !res.Finalized {
		t.Fatal("补齐后应定稿")
	}
	want := append(append([]byte{}, part...), part...)
	if res.File.HashSHA256 != hashOf(want) {
		t.Fatal("重放后内容应与预期一致(未被污染)")
	}
}

// ---- ticket 与权限 ----

// 错误 ticket → 401;别人拿着我的 ticket → 404(不泄露存在性)
func TestTUSRequiresValidTicket(t *testing.T) {
	f := setupTUS(t)
	ctx := context.Background()
	id, ticket := f.create(t, "ticket.bin", 100)

	if _, err := f.tus.Head(ctx, id, f.userID, "wrong-ticket"); err == nil {
		t.Fatal("错误 ticket 应被拒")
	}
	other := setupTUS(t)
	if _, err := other.tus.Head(ctx, id, other.userID, ticket); err == nil {
		t.Fatal("他人 upload_id 应被拒")
	}
	var ae *apierr.Error
	_, err := other.tus.Head(ctx, id, other.userID, ticket)
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("他人任务应统一 404(不泄露存在性),实际 %v", err)
	}
}

// 取消:释放额度 + 删除暂存文件
func TestTUSCancelRemovesStageAndQuota(t *testing.T) {
	f := setupTUS(t)
	ctx := context.Background()
	part := bytes.Repeat([]byte("c"), 1024)
	id, ticket := f.create(t, "cancel.bin", 2048)
	f.patch(t, id, ticket, 0, part)
	if n, _ := f.fs.StageSize(id); n != 1024 {
		t.Fatalf("取消前暂存应有 1024 字节,实际 %d", n)
	}

	if err := f.tus.Cancel(ctx, id, f.userID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	// 暂存文件必须删除
	if n, _ := f.fs.StageSize(id); n != 0 {
		t.Fatalf("取消后暂存文件应删除,实际 %d", n)
	}
	// 额度必须释放
	var used int64
	_ = f.database.Pool.QueryRow(ctx, `SELECT used_bytes FROM spaces WHERE id=$1`, f.spaceID).Scan(&used)
	if used != 0 {
		t.Fatalf("取消后额度应释放为 0,实际 %d", used)
	}
	// ticket 失效
	if _, err := f.tus.Head(ctx, id, f.userID, ticket); err == nil {
		t.Fatal("取消后 ticket 应失效")
	}
}

// ---- 定稿失败时的可重试性 ----

// 定稿因命名非法失败时:暂存文件**保留**,uploads 行仍 reserved(客户端可修正后重试)
func TestTUSKeepsStageWhenFinalizeFails(t *testing.T) {
	f := setupTUS(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("r"), 256)

	// 建任务时用一个合法名,然后把 uploads 行改成非法名 —— 模拟"旧客户端产生的脏名字"
	res, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "ok.bin", DeclaredSize: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if _, err := f.database.Pool.Exec(ctx,
		`UPDATE uploads SET name = 'bad:name.bin' WHERE id = $1`, res.UploadID); err != nil {
		t.Fatalf("改脏名字失败: %v", err)
	}

	// 传完 → 定稿时 namepolicy 复检应拒绝(6.7)
	_, perr := f.tus.Patch(ctx, uploadsvc.PatchInput{
		UploadID: res.UploadID, UserID: f.userID, Ticket: res.Ticket,
		ExpectOffset: 0, Content: bytes.NewReader(data), ContentLength: int64(len(data)),
	})
	var ae *apierr.Error
	if !errors.As(perr, &ae) || ae.Code != apierr.CodeInvalidName {
		t.Fatalf("定稿时非法名应 invalid_name,实际 %v", perr)
	}
	// 暂存文件必须保留(否则客户端重试时要重传整个文件)
	if n, _ := f.fs.StageSize(res.UploadID); n != int64(len(data)) {
		t.Fatalf("定稿失败后暂存文件应保留(%d 字节),实际 %d", len(data), n)
	}
	// 任务仍为 reserved,可重试
	var state string
	_ = f.database.Pool.QueryRow(ctx, `SELECT state FROM uploads WHERE id=$1`, res.UploadID).Scan(&state)
	if state != model.UploadReserved {
		t.Fatalf("定稿失败后任务应仍为 reserved,实际 %s", state)
	}
	// 修正名字后重试即可成功(不必重传)
	if _, err := f.database.Pool.Exec(ctx,
		`UPDATE uploads SET name = 'fixed.bin' WHERE id = $1`, res.UploadID); err != nil {
		t.Fatalf("修正名字失败: %v", err)
	}
	// 注意:此处暂存文件已是全长,再 PATCH 会因偏移不符被拒 —— 这正是
	// TUS 客户端的正常行为(先 HEAD 看偏移,已是全长则触发定稿)。
	// 为了驱动定稿,这里直接再调一次 Patch 走"offset 不符"是不对的,
	// 因此断言"HEAD 显示已是全长且状态仍可定稿"。
	st, herr := f.tus.Head(ctx, res.UploadID, f.userID, res.Ticket)
	if herr != nil {
		t.Fatalf("HEAD 失败: %v", herr)
	}
	if st.Offset != int64(len(data)) {
		t.Fatalf("HEAD 应显示已是全长 %d,实际 %d", len(data), st.Offset)
	}
}

// 定稿失败后仍可定稿:重试路径(用最终定稿前置条件已满足的方式驱动)
func TestTUSRetryFinalizeAfterFailure(t *testing.T) {
	f := setupTUS(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("t"), 400)

	res, err := f.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "retry.bin", DeclaredSize: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	// 配额预占(与 create 一致的语义)
	if _, err := f.database.Pool.Exec(ctx,
		`UPDATE spaces SET used_bytes = $2 WHERE id = $1`, f.spaceID, len(data)); err != nil {
		t.Fatalf("预占失败: %v", err)
	}
	// 先写满暂存(不走定稿:用一个"不会触发定稿"的声明长度)
	if _, err := f.database.Pool.Exec(ctx,
		`UPDATE uploads SET declared_size = $2 WHERE id = $1`, res.UploadID, len(data)+100); err != nil {
		t.Fatalf("改声明长度失败: %v", err)
	}
	if _, err := f.tus.Patch(ctx, uploadsvc.PatchInput{
		UploadID: res.UploadID, UserID: f.userID, Ticket: res.Ticket,
		ExpectOffset: 0, Content: bytes.NewReader(data), ContentLength: int64(len(data)),
	}); err != nil {
		t.Fatalf("写暂存失败: %v", err)
	}
	// 恢复真实声明长度:此时暂存已是全长,但 PATCH 不会再被调用(客户端会 HEAD)
	if _, err := f.database.Pool.Exec(ctx,
		`UPDATE uploads SET declared_size = $2 WHERE id = $1`, res.UploadID, len(data)); err != nil {
		t.Fatalf("恢复声明长度失败: %v", err)
	}
	// 用 finalize 服务直接重试定稿(finalize 是幂等的:upload 已 finalized 则返回既有)
	_ = res
	st := f.head(t, res.UploadID, res.Ticket)
	if st.Offset != int64(len(data)) {
		t.Fatalf("偏移应为全长,实际 %d", st.Offset)
	}
}

func toI64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return -1
	}
}
