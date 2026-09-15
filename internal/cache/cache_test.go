package cache_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/cache"
)

func newClient(t *testing.T) (*cache.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return cache.New(rdb, "netdisk:"), mr
}

// 6.7:Redis key 只用 id,不得含文件名等自由文本
func TestKeyBuilderRejectsFreeText(t *testing.T) {
	c, _ := newClient(t)

	ok := []struct {
		parts []string
		want  string
	}{
		{[]string{"auth", "refresh", "11111111-1111-7111-8111-111111111111", "abc123"}, "netdisk:auth:refresh:11111111-1111-7111-8111-111111111111:abc123"},
		{[]string{"rl", "upload", "u", "42", "s"}, "netdisk:rl:upload:u:42:s"},
		{[]string{"space", "abc", "member", "def"}, "netdisk:space:abc:member:def"},
	}
	for _, tc := range ok {
		got, err := c.Key(tc.parts...)
		if err != nil {
			t.Errorf("Key(%v) 不应报错: %v", tc.parts, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Key(%v)=%q want %q", tc.parts, got, tc.want)
		}
	}

	bad := [][]string{
		{"file", "月/报告.xlsx"},              // 中文+斜杠
		{"file", "a b.txt"},                // 空格
		{"file", "报告"},                     // 中文
		{"file", strings.Repeat("x", 200)}, // 超长
		{},                                 // 空
		{"path", "a/b"},                    // 斜杠
	}
	for _, parts := range bad {
		if got, err := c.Key(parts...); err == nil {
			t.Errorf("Key(%v) 应被拒绝,却得到 %q", parts, got)
		}
	}
}

func TestSetGetDel(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	key, _ := c.Key("user", "u1", "tv")

	if err := c.Set(ctx, key, 7, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, found, err := c.GetInt64(ctx, key)
	if err != nil || !found || v != 7 {
		t.Fatalf("GetInt64 = %d,%v,%v", v, found, err)
	}
	if err := c.Del(ctx, key); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, found, _ := c.GetInt64(ctx, key); found {
		t.Error("删除后不应再读到")
	}
}

func TestIncrSetsTTL(t *testing.T) {
	c, mr := newClient(t)
	ctx := context.Background()
	key, _ := c.Key("login", "fail", "1.2.3.4")

	n1, err := c.Incr(ctx, key, 10*time.Minute)
	if err != nil || n1 != 1 {
		t.Fatalf("首次 Incr = %d, %v", n1, err)
	}
	n2, _ := c.Incr(ctx, key, 10*time.Minute)
	if n2 != 2 {
		t.Fatalf("二次 Incr = %d", n2)
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > 10*time.Minute {
		t.Errorf("TTL 应为正且 <= 10m, got %v", ttl)
	}
}

// Redis 不可用时必须返回 ErrUnavailable,便于上层决定"拒绝/降级"(10.7)
func TestUnavailable(t *testing.T) {
	c := cache.New(nil, "netdisk:")
	ctx := context.Background()
	key, _ := c.Key("x", "y")
	if err := c.Set(ctx, key, 1, time.Minute); err == nil {
		t.Error("未配置 Redis 时 Set 应报错")
	}
	if err := c.Ping(ctx); err == nil {
		t.Error("未配置 Redis 时 Ping 应报错")
	}
}

func TestSetNX(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	key, _ := c.Key("lock", "job1")
	first, err := c.SetNX(ctx, key, 1, time.Minute)
	if err != nil || !first {
		t.Fatalf("首次 SetNX 应成功: %v %v", first, err)
	}
	second, _ := c.SetNX(ctx, key, 1, time.Minute)
	if second {
		t.Error("第二次 SetNX 应失败")
	}
}
