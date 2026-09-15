package webdavauth_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/webdavauth"
)

// RedisStore 用 miniredis 验证(真 Redis 的语义由 miniredis 覆盖:
// 过期、SET/GET/DEL;本机无 Docker,不引入外部依赖)。

func newRedisStore(t *testing.T, prefix string) (*webdavauth.RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &webdavauth.RedisStore{Cache: cache.New(rdb, prefix), TTL: 12 * time.Hour}, mr
}

func TestRedisStoreRoundTrip(t *testing.T) {
	st, _ := newRedisStore(t, "netdisk:")
	ctx := context.Background()
	const hash = "abc123"

	if err := st.Put(ctx, hash, "user-1", time.Hour, 42); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	userID, tv, ok, err := st.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if !ok || userID != "user-1" || tv != 42 {
		t.Fatalf("读回值不对: ok=%v user=%q tv=%d", ok, userID, tv)
	}
}

func TestRedisStoreMissingIsNotFoundNotError(t *testing.T) {
	st, _ := newRedisStore(t, "netdisk:")
	_, _, ok, err := st.Get(context.Background(), "never-written")
	if err != nil {
		t.Fatalf("未命中的 key 不应报错(否则 Redis 抖动会被误判为凭据问题): %v", err)
	}
	if ok {
		t.Fatal("未命中的 key 应返回 ok=false")
	}
}

func TestRedisStoreDelete(t *testing.T) {
	st, _ := newRedisStore(t, "netdisk:")
	ctx := context.Background()
	if err := st.Put(ctx, "h1", "u1", time.Hour, 1); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	if err := st.Delete(ctx, "h1"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if _, _, ok, _ := st.Get(ctx, "h1"); ok {
		t.Fatal("删除后不应命中")
	}
	// 删除不存在的 key 不应报错(幂等)
	if err := st.Delete(ctx, "never"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
}

// 过期后会话必须失效(短时效令牌的核心保证)。
func TestRedisStoreExpires(t *testing.T) {
	st, mr := newRedisStore(t, "netdisk:")
	ctx := context.Background()
	if err := st.Put(ctx, "h2", "u1", 90*time.Second, 3); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	mr.FastForward(91 * time.Second)
	if _, _, ok, err := st.Get(ctx, "h2"); err != nil || ok {
		t.Fatalf("过期后应未命中且无错误: ok=%v err=%v", ok, err)
	}
}

// 值格式被写坏时必须显式报错(便于排查),而不是静默当作"密码错误"。
func TestRedisStoreCorruptValueReported(t *testing.T) {
	st, mr := newRedisStore(t, "netdisk:")
	ctx := context.Background()
	if err := st.Put(ctx, "h3", "u1", time.Hour, 1); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	// 找到真实 key 并改坏它
	keys := mr.Keys()
	if len(keys) != 1 {
		t.Fatalf("期望 1 个 key,实际 %d: %v", len(keys), keys)
	}
	mr.Set(keys[0], "garbage-without-colon")

	if _, _, _, err := st.Get(ctx, "h3"); err == nil {
		t.Fatal("坏值必须报错,不能静默当作凭据无效")
	}
}

// key 前缀必须生效(多环境/多实例共用 Redis 时不能互相串号)。
func TestRedisStoreHonorsKeyPrefix(t *testing.T) {
	st, mr := newRedisStore(t, "netdisk:test:")
	if err := st.Put(context.Background(), "h4", "u1", time.Hour, 1); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	keys := mr.Keys()
	if len(keys) != 1 {
		t.Fatalf("期望 1 个 key,实际 %v", keys)
	}
	if got := keys[0]; len(got) < len("netdisk:test:") || got[:len("netdisk:test:")] != "netdisk:test:" {
		t.Fatalf("key 应带前缀 netdisk:test:,实际 %q", got)
	}
}

// 未装配 Cache 时必须报错,不能静默通过。
func TestRedisStoreUnassembledFailsClosed(t *testing.T) {
	st := &webdavauth.RedisStore{}
	if err := st.Put(context.Background(), "h", "u", time.Hour, 1); err == nil {
		t.Fatal("未装配 Cache 时 Put 应报错")
	}
	if _, _, _, err := st.Get(context.Background(), "h"); err == nil {
		t.Fatal("未装配 Cache 时 Get 应报错")
	}
}
