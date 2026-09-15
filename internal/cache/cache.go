// Package cache 封装 Redis 访问与**键命名规范**。
//
// 依据架构文档 6.7「审计/日志/缓存」一行:Redis key、监控 label 只用 id 不用名。
// 因此本包提供的 KeyBuilder 只接受 id/UUID/枚举,不接受文件名、邮箱等自由文本。
//
// 10.7 降级约定:Redis 不可用时
//   - 认证必须拒绝(不得降级放行)
//   - SSE 断开 → 客户端自动退 /api/v1/changes 轮询
//   - 限速退 Nginx 层兜底
package cache

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrUnavailable 表示 Redis 不可用(供上层决定"拒绝"还是"降级")。
var ErrUnavailable = errors.New("cache: redis unavailable")

// idPattern 只允许:UUID、数字 id、字母数字下划线短串(枚举)。
// 刻意排除空格/中文/斜杠/冒号,防止把文件名或路径拼进 key。
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// Client 是带前缀与健康检查的 Redis 封装。
type Client struct {
	rdb    *redis.Client
	prefix string
}

// New 构造;prefix 来自 config(默认 netdisk:)。
func New(rdb *redis.Client, prefix string) *Client {
	if prefix == "" {
		prefix = "netdisk:"
	}
	return &Client{rdb: rdb, prefix: prefix}
}

// Raw 暴露底层客户端(仅供需要 pub/sub、pipeline 的模块使用)。
func (c *Client) Raw() *redis.Client { return c.rdb }

// Ping 供启动自检与 /healthz 使用。
func (c *Client) Ping(ctx context.Context) error {
	if c.rdb == nil {
		return ErrUnavailable
	}
	return c.rdb.Ping(ctx).Err()
}

// ---- 键命名(唯一入口,禁止业务代码手拼字符串)----

// Key 拼装并校验 key。
//
// 用法:key, err := c.Key("auth", "refresh", userID, jti)
// 产出:netdisk:auth:refresh:<userID>:<jti>
func (c *Client) Key(parts ...string) (string, error) {
	if len(parts) == 0 {
		return "", errors.New("cache: empty key parts")
	}
	out := c.prefix
	for i, p := range parts {
		if !idPattern.MatchString(p) {
			return "", fmt.Errorf("cache: 非法 key 片段 %q(只允许 id/枚举;禁止文件名等自由文本)", p)
		}
		if i > 0 {
			out += ":"
		}
		out += p
	}
	return out, nil
}

// ---- 基础操作(带 TTL 语义)----

// Set 写入并设置 TTL;ttl<=0 视为永久(不推荐,仅用于计数器)。
func (c *Client) Set(ctx context.Context, key string, val any, ttl time.Duration) error {
	if c.rdb == nil {
		return ErrUnavailable
	}
	return c.rdb.Set(ctx, key, val, ttl).Err()
}

// GetString 读取;不存在返回 ("", false, nil),便于"未命中不算错误"的调用方。
func (c *Client) GetString(ctx context.Context, key string) (string, bool, error) {
	if c.rdb == nil {
		return "", false, ErrUnavailable
	}
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// GetInt64 读取整数;不存在返回 (0, false, nil)。
func (c *Client) GetInt64(ctx context.Context, key string) (int64, bool, error) {
	if c.rdb == nil {
		return 0, false, ErrUnavailable
	}
	v, err := c.rdb.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

// Del 删除一个或多个 key。
func (c *Client) Del(ctx context.Context, keys ...string) error {
	if c.rdb == nil {
		return ErrUnavailable
	}
	if len(keys) == 0 {
		return nil
	}
	return c.rdb.Del(ctx, keys...).Err()
}

// Incr 自增并设置首次 TTL(用于简单计数,如登录失败次数)。
func (c *Client) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	if c.rdb == nil {
		return 0, ErrUnavailable
	}
	pipe := c.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// SetNX 仅当不存在时写入,返回是否写入成功(用于分布式锁/去重)。
func (c *Client) SetNX(ctx context.Context, key string, val any, ttl time.Duration) (bool, error) {
	if c.rdb == nil {
		return false, ErrUnavailable
	}
	return c.rdb.SetNX(ctx, key, val, ttl).Result()
}

// ---- 常用键(集中定义,避免各处手拼)----

// TokenVersionKey 用户令牌版本缓存(与 users.token_version 比对,6.8 纪律 3)。
func (c *Client) TokenVersionKey(userID string) (string, error) { return c.Key("user", userID, "tv") }

// SpacePermKey 空间成员权限缓存(4.3:成员变更即时生效)。
func (c *Client) SpacePermKey(spaceID, userID string) (string, error) {
	return c.Key("space", spaceID, "member", userID)
}

// SyncCursorReportKey 客户端游标上报的 Redis 聚合桶(6.4:权威值每日回写 PG)。
func (c *Client) SyncCursorReportKey() (string, error) { return c.Key("sync", "cursor_report") }

// IdempotencyKey 上传定稿幂等(6.10:重复 finish 返回既有 file_id)。
func (c *Client) IdempotencyKey(uploadID string) (string, error) {
	return c.Key("upload", uploadID, "finalized")
}

// LockKey 通用互斥键(仅用于与业务流程无关的短锁)。
func (c *Client) LockKey(name string) (string, error) { return c.Key("lock", name) }
