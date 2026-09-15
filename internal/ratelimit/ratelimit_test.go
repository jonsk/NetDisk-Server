package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/ratelimit"
)

func newLimiter(t *testing.T) (*ratelimit.Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return ratelimit.New(rdb), mr
}

// 固定窗口:窗口内超出 Max 即拒绝并给出 Retry-After;进入新窗口后恢复
func TestFixedWindowRejectAndReset(t *testing.T) {
	l, _ := newLimiter(t)
	ctx := context.Background()
	lim := ratelimit.Limit{Window: time.Second, Max: 3}

	now := time.Now()
	l.SetClock(func() time.Time { return now })

	for i := 1; i <= 3; i++ {
		res, err := l.Allow(ctx, "k1", lim, 1)
		if err != nil || !res.Allowed {
			t.Fatalf("第 %d 次应放行: %+v %v", i, res, err)
		}
		if want := int64(3 - i); res.Remaining != want {
			t.Errorf("第 %d 次 remaining=%d want %d", i, res.Remaining, want)
		}
	}
	res, err := l.Allow(ctx, "k1", lim, 1)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if res.Allowed {
		t.Fatal("第 4 次应被拒绝(窗口已满)")
	}
	if res.Remaining != 0 {
		t.Errorf("拒绝时 remaining 应为 0,got %d", res.Remaining)
	}
	// 重试时间应落在 (0, 1s]
	if res.RetryAfter <= 0 || res.RetryAfter > time.Second {
		t.Errorf("Retry-After 应落在 (0,1s], got %v", res.RetryAfter)
	}

	// 进入下一窗口 → 重新计数
	now = now.Add(time.Second)
	if res, _ := l.Allow(ctx, "k1", lim, 1); !res.Allowed {
		t.Fatalf("新窗口应放行: %+v", res)
	}
	// 窗口序号必须变化(用于排查"计数不重置"这类问题)
	if res, _ := l.Allow(ctx, "k1", lim, 1); res.WindowIndex == 0 {
		t.Error("WindowIndex 应非 0")
	}
}

// 不同 key 互不影响
func TestIsolationBetweenKeys(t *testing.T) {
	l, _ := newLimiter(t)
	ctx := context.Background()
	// **注入固定时钟**:窗口只有 1 秒,而这里要连续发三次请求。
	// 用真实时钟时,机器一忙(全量并行测试)第三次就可能跨进下一个窗口 ——
	// 计数器被重置、请求被放行,报成"A 第二次应被拒"的假失败(实测遇到过一次)。
	// 限速用例只要涉及"窗口内第几次",就必须把时间钉住。
	now := time.Now()
	l.SetClock(func() time.Time { return now })
	lim := ratelimit.Limit{Window: time.Second, Max: 1}
	if res, _ := l.Allow(ctx, "userA", lim, 1); !res.Allowed {
		t.Fatal("A 首请求应放行")
	}
	if res, _ := l.Allow(ctx, "userB", lim, 1); !res.Allowed {
		t.Fatal("B 不应受 A 影响")
	}
	if res, _ := l.Allow(ctx, "userA", lim, 1); res.Allowed {
		t.Fatal("A 第二次应被拒")
	}
}

// Redis 不可用时的两种策略
func TestFailOpenVsFailClose(t *testing.T) {
	ctx := context.Background()
	lim := ratelimit.Limit{Window: time.Second, Max: 1}

	open := ratelimit.New(nil)
	open.FailOpen = true
	if res, err := open.Allow(ctx, "k", lim, 1); err != nil || !res.Allowed {
		t.Fatalf("FailOpen 应放行: %+v %v", res, err)
	}

	closed := ratelimit.New(nil)
	closed.FailOpen = false
	if res, _ := closed.Allow(ctx, "k", lim, 1); res.Allowed {
		t.Fatal("FailClose 应拒绝")
	}
}

// 零阈值视为不限制
func TestZeroLimitMeansUnlimited(t *testing.T) {
	l, _ := newLimiter(t)
	for i := 0; i < 50; i++ {
		if res, _ := l.Allow(context.Background(), "k", ratelimit.Limit{}, 1); !res.Allowed {
			t.Fatal("未配置阈值时应放行")
		}
	}
}

// 窗口 TTL 必须被设置,否则计数永不重置
func TestWindowKeyHasTTL(t *testing.T) {
	l, mr := newLimiter(t)
	lim := ratelimit.Limit{Window: 2 * time.Second, Max: 5}
	res, _ := l.Allow(context.Background(), "k9", lim, 1)
	key := "k9:" + itoa(res.WindowIndex)
	ttl := mr.TTL(key)
	if ttl <= 0 || ttl > 2*time.Second {
		t.Fatalf("窗口键必须带 TTL 且 <= 窗口长度, got %v (key=%s)", ttl, key)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
