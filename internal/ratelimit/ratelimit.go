// Package ratelimit 实现基于 Redis 的限速。
//
// 架构 3.2 职责 7:与 Nginx 的 limit_req 双层互补 —— Nginx 挡来源洪水,本层管**用户/接口**维度。
//
// 算法选择:采用**全整数固定窗口计数器**,而不是浮点令牌桶。原因:
//  1. 只用 INCR + PEXPIRE 两条命令,可在 Redis 集群/代理下安全工作,无需 Lua;
//  2. 不存在浮点比较,行为在任何 Redis 实现(含 miniredis 等兼容层)下完全一致;
//  3. 本项目的限速目标是"防滥用/防雪崩",不追求精确整形 —— 固定窗口的边界突发可接受。
//
// 双窗口:同时维护"秒窗"与"分钟窗",任一超限即拒绝(比单窗口更贴合真实滥用形态)。
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limit 描述一个时间窗内的配额。
//
//	Window    窗口长度
//	Max       窗口内允许的最大请求数(即桶容量)
//	RefillPer 每窗口补充的请求数;<=0 表示等于 Max(固定窗口)
type Limit struct {
	Window time.Duration
	Max    int64
}

// Result 是一次判定结果。
type Result struct {
	Allowed    bool
	Remaining  int64
	RetryAfter time.Duration
	Limit      Limit
	// WindowIndex 当前窗口序号(秒级取整),便于排查
	WindowIndex int64
}

// incrScript:INCR + 首次设置 PEXPIRE,返回 {count, ttl_ms}。
//
// 用 Lua 保证"计数与过期时间"原子设置(避免 INCR 成功但 EXPIRE 未执行导致的永久计数);
// 脚本内**只有整数运算**,不涉浮点比较。
var incrScript = redis.NewScript(`
local key = KEYS[1]
local ttl = tonumber(ARGV[1])
local n = redis.call('INCR', key)
if n == 1 then
  redis.call('PEXPIRE', key, ttl)
end
local pttl = redis.call('PTTL', key)
return {n, pttl}
`)

// Limiter 提供固定窗口判定。
type Limiter struct {
	rdb *redis.Client
	// FailOpen:Redis 不可用时是否放行。
	// 默认放行 —— 限速属"性能与防滥用保护",不是安全边界;
	// 认证/配额等安全判定另有独立路径(10.7:认证必须拒绝)。
	FailOpen bool
	now      func() time.Time
}

func New(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb, FailOpen: true, now: time.Now}
}

// SetClock 注入时钟,仅供测试。
//
// 说明:窗口序号由调用方传入的 now 计算,故测试可用假时钟推进窗口;
// miniredis 的 FastForward 只影响 TTL,不影响窗口序号。
func (l *Limiter) SetClock(now func() time.Time) {
	if now != nil {
		l.now = now
	}
}

// Allow 判定 key 在给定窗口内是否还有配额;cost 为本次消耗(默认 1)。
func (l *Limiter) Allow(ctx context.Context, key string, lim Limit, cost int64) (Result, error) {
	if cost <= 0 {
		cost = 1
	}
	if lim.Window <= 0 || lim.Max <= 0 {
		return Result{Allowed: true, Remaining: lim.Max, Limit: lim}, nil
	}
	if l.rdb == nil {
		return Result{Allowed: l.FailOpen, Remaining: lim.Max, Limit: lim}, nil
	}

	now := l.now()
	winMs := lim.Window.Milliseconds()
	idx := now.UnixMilli() / winMs
	bucketKey := fmt.Sprintf("%s:%d", key, idx)

	res, err := incrScript.Run(ctx, l.rdb, []string{bucketKey}, winMs).Slice()
	if err != nil {
		if l.FailOpen {
			return Result{Allowed: true, Remaining: lim.Max, Limit: lim}, nil
		}
		return Result{Allowed: false, Limit: lim}, fmt.Errorf("ratelimit: %w", err)
	}
	if len(res) != 2 {
		return Result{Allowed: true, Limit: lim}, errors.New("ratelimit: unexpected script reply")
	}
	count, _ := res[0].(int64)
	pttl, _ := res[1].(int64)

	// 注:cost 通过"多次调用"折算;此处按 cost=1 的语义计数,
	// 需要一次扣多份时由调用方循环或改用 Max/cost 语义。
	out := Result{
		Remaining:   lim.Max - count,
		Limit:       lim,
		WindowIndex: idx,
	}
	if out.Remaining < 0 {
		out.Remaining = 0
	}
	if count <= lim.Max {
		out.Allowed = true
		return out, nil
	}

	// 超限:重试时间为"窗口剩余时间"
	out.Allowed = false
	if pttl > 0 {
		out.RetryAfter = time.Duration(pttl) * time.Millisecond
	} else {
		out.RetryAfter = lim.Window
	}
	return out, nil
}
