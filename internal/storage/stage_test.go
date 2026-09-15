package storage_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netdisk/netdisk/internal/storage"
)

// TUS 暂存区(6.1 分片暂存)的不变量。

func TestStageAppendAndSize(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	const id = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"

	// 不存在时长度为 0(不报错)—— HEAD 首次探测依赖这个语义
	if n, err := fs.StageSize(id); err != nil || n != 0 {
		t.Fatalf("不存在时 StageSize 应为 (0,nil),实际 (%d,%v)", n, err)
	}

	n, err := fs.StageAppend(ctx, id, 0, bytes.NewReader([]byte("hello ")))
	if err != nil {
		t.Fatalf("首次追加失败: %v", err)
	}
	if n != 6 {
		t.Fatalf("追加后长度应为 6,实际 %d", n)
	}
	n, err = fs.StageAppend(ctx, id, 6, bytes.NewReader([]byte("world")))
	if err != nil {
		t.Fatalf("续传失败: %v", err)
	}
	if n != 11 {
		t.Fatalf("续传后长度应为 11,实际 %d", n)
	}
	// 内容必须是按序拼接的结果
	fh, err := fs.StageOpen(id)
	if err != nil {
		t.Fatalf("StageOpen 失败: %v", err)
	}
	defer func() { _ = fh.Close() }()
	buf := make([]byte, 16)
	got, _ := fh.Read(buf)
	if string(buf[:got]) != "hello world" {
		t.Fatalf("暂存内容应为 %q,实际 %q", "hello world", string(buf[:got]))
	}
	// StageSize 与文件一致
	if n, _ := fs.StageSize(id); n != 11 {
		t.Fatalf("StageSize 应为 11,实际 %d", n)
	}
}

// **偏移不符必须拒绝**:少了这一步,乱序/重放分片会被写到错误位置 → 内容损坏,
// 而哈希要到定稿才发现 —— 那时用户已经传完了整个文件。
func TestStageAppendRejectsWrongOffset(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	const id = "0199a1b2-c3d4-7e5f-8a9b-000000000001"

	if _, err := fs.StageAppend(ctx, id, 0, bytes.NewReader([]byte("0123456789"))); err != nil {
		t.Fatalf("首次追加失败: %v", err)
	}
	// 重放(偏移 0 < 长度 10)
	n, err := fs.StageAppend(ctx, id, 0, bytes.NewReader([]byte("XXX")))
	if !errors.Is(err, storage.ErrOffsetMismatch) {
		t.Fatalf("重放应 ErrOffsetMismatch,实际 %v", err)
	}
	// 返回真实长度,供上层放进 409 响应头让客户端自我纠正
	if n != 10 {
		t.Fatalf("冲突时应返回真实长度 10,实际 %d", n)
	}
	// 跳跃(偏移 5 < 长度 10)
	if _, err := fs.StageAppend(ctx, id, 5, bytes.NewReader([]byte("X"))); !errors.Is(err, storage.ErrOffsetMismatch) {
		t.Fatalf("跳跃应 ErrOffsetMismatch,实际 %v", err)
	}
	// 内容不得被污染
	if sz, _ := fs.StageSize(id); sz != 10 {
		t.Fatalf("冲突后长度应仍为 10,实际 %d", sz)
	}
}

// upload_id 会拼进文件路径 → 必须严格校验(路径穿越、保留名一律拒绝)
func TestStagePathRejectsUnsafeUploadID(t *testing.T) {
	fs := newFS(t)
	for _, bad := range []string{
		"", "..", "../etc/passwd", "a/b", `a\b`, "a..b/c", "con", "name with space",
		"0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b/../x", strings.Repeat("a", 65), "------",
	} {
		if _, err := fs.StagePath(bad); err == nil {
			t.Errorf("非法 upload_id %q 应被拒绝", bad)
		}
	}
	// 合法 UUID 形态可以通过
	if _, err := fs.StagePath("0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"); err != nil {
		t.Fatalf("合法 UUID 应被接受: %v", err)
	}
}

// 暂存文件必须落在 tus-tmp 目录(运维只需盯一个目录做清理与容量监控)
func TestStageLivesInTusTmp(t *testing.T) {
	fs := newFS(t)
	const id = "0199a1b2-c3d4-7e5f-8a9b-000000000002"
	p, err := fs.StagePath(id)
	if err != nil {
		t.Fatalf("StagePath 失败: %v", err)
	}
	if filepath.Base(filepath.Dir(p)) != "tus-tmp" {
		t.Fatalf("暂存文件应在 tus-tmp 下,实际 %s", p)
	}
	if filepath.Base(p) != id+".chunk" {
		t.Fatalf("暂存文件名应为 <upload_id>.chunk,实际 %s", filepath.Base(p))
	}
}

func TestStageRemoveIsIdempotent(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	const id = "0199a1b2-c3d4-7e5f-8a9b-000000000003"
	if _, err := fs.StageAppend(ctx, id, 0, bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	if err := fs.StageRemove(id); err != nil {
		t.Fatalf("首次删除失败: %v", err)
	}
	// 清理任务会重试删除:不存在不算错误
	if err := fs.StageRemove(id); err != nil {
		t.Fatalf("重复删除必须幂等: %v", err)
	}
	if _, err := fs.StageOpen(id); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("删除后 StageOpen 应 ErrNotFound,实际 %v", err)
	}
}

func TestListStagesSkipsUnrecognized(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	good := "0199a1b2-c3d4-7e5f-8a9b-000000000004"
	if _, err := fs.StageAppend(ctx, good, 0, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	dir, err := fs.StageDir()
	if err != nil {
		t.Fatalf("StageDir 失败: %v", err)
	}
	// 混入不认识的文件:清理任务**不得**把它们当暂存文件
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("造干扰文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "乱码.chunk"), []byte("x"), 0o600); err != nil {
		t.Fatalf("造干扰文件失败: %v", err)
	}

	ids, err := fs.ListStages()
	if err != nil {
		t.Fatalf("ListStages 失败: %v", err)
	}
	if len(ids) != 1 || ids[0] != good {
		t.Fatalf("应只列出合法暂存文件 %s,实际 %v", good, ids)
	}
}
