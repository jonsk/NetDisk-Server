package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 测试装配小工具:auth_test/upload_test 共用,避免每个文件重复一遍 miniredis 样板。

// testConfig 返回测试用配置(固定 JWT 密钥,日志静音)。
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.JWT.Secret = testSecret
	cfg.Log.Level = "error"
	return cfg
}

// testLogger 返回丢弃式 logger(测试输出保持干净)。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTokens 基于内存 Redis 构造令牌管理器,并注册清理。
func newTokens(t *testing.T, cfg *config.Config) *auth.Manager {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return auth.NewManager(cfg.JWT, rdb, cfg.Redis.KeyPrefix)
}

// newCache 构造独立的内存 Redis 缓存客户端(限速、WebDAV 会话等测试用)。
func newCache(t *testing.T, prefix string) *cache.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return cache.New(rdb, prefix)
}

// doJSONReq 发一个带 Bearer 令牌的 JSON 请求(多个测试文件共用)。
//
// body 为 nil 时不带请求体与 Content-Type。
func doJSONReq(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// newUploadSvc 构造上传用例服务(测试用,与生产的装配保持同一套依赖)。
func newUploadSvc(database *db.DB) *uploadsvc.Service {
	return &uploadsvc.Service{
		Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, Uploads: repo.UploadRepo{},
		DB:   db.AsQuerier(database),
		Name: namepolicy.Default(),
	}
}

// newFinalizer 构造定稿服务(测试用)。
//
// 必须用**同一个** storage 实例:定稿写入的对象要能被后续的下载端点读到,
// 用不同的临时目录会得到"对象不存在"的假失败。
func newFinalizer(t *testing.T, database *db.DB, store *storage.FS) *finalize.Service {
	t.Helper()
	return &finalize.Service{
		Pool: database.Pool, Storage: store,
		Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{},
		Name: namepolicy.Default(),
	}
}

// jsonUnmarshal 是测试内的小包装(避免每个测试文件都 import encoding/json)。
func jsonUnmarshal(b []byte, dst any) error { return json.Unmarshal(b, dst) }

// deptTestLockKey 与 orgsvc 包使用同一个 advisory lock key("netd")。//
// 部门树的闭包表重建是**全库**操作(DELETE 全表 + 全量 INSERT),而 Go 的
// 集成测试包之间是并行执行的。两个包同时全库重建会互相删掉对方的数据,
// 表现为"单独跑某包必过、全量跑随机挂"的假失败。用 PG 会话级 advisory lock
// 把两个包串行化;锁随连接关闭自动释放,测试崩溃也不会留下死锁。
const deptTestLockKey = 0x6E657464

// lockDeptTests 获取部门树测试的全局互斥锁(测试结束时释放)。
func lockDeptTests(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("获取测试锁连接失败: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, deptTestLockKey); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("获取测试锁失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = conn.Exec(bg, `SELECT pg_advisory_unlock($1)`, deptTestLockKey)
		_ = conn.Close(bg)
	})
}
