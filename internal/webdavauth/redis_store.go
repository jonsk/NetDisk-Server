package webdavauth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/netdisk/netdisk/internal/cache"
)

// RedisStore 是 Store 的生产实现。
//
// 存储布局(全部走 cache.Client 的强制 key 规范,6.7:key 里绝不出现文件名):
//
//	netdisk:webdav:s:<tokenHash>  →  "userID:tokenVersion",TTL = 会话有效期
//
// 为什么把 tokenVersion 和 userID 放同一个字符串而不是 Hash:
// 这是一次 GET 就能拿全的热路径(每个 WebDAV 请求都走),Hash 需要 HGETALL 或两次调用,
// 而两者永远同时读写、没有单独更新的需求。
type RedisStore struct {
	Cache *cache.Client
	// TTL 与 Authenticator.TTL 保持一致(此处兜底,避免只装配 Store 时 key 永不过期)
	TTL time.Duration
}

// ErrNoStore 表示未装配 Cache。
var ErrNoStore = errors.New("webdavauth: 未装配 Redis 缓存")

// key 生成会话键。
//
// cache.Client.Key 会对每段做字符白名单校验(拒绝文件名/中文/空格/冒号,6.7),
// 这里传入的是 sha256 hex,天然合规;若校验失败说明调用方传错了东西,直接返回错误。
func (s *RedisStore) key(tokenHash string) (string, error) {
	return s.Cache.Key("webdav", "s", tokenHash)
}

// Put 写入会话。
func (s *RedisStore) Put(ctx context.Context, tokenHash, userID string, ttl time.Duration, tokenVersion int64) error {
	if s.Cache == nil {
		return ErrNoStore
	}
	if ttl <= 0 {
		ttl = s.TTL
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	k, err := s.key(tokenHash)
	if err != nil {
		return fmt.Errorf("生成 WebDAV 会话键失败: %w", err)
	}
	val := userID + ":" + strconv.FormatInt(tokenVersion, 10)
	if err := s.Cache.Set(ctx, k, val, ttl); err != nil {
		return fmt.Errorf("写入 WebDAV 会话失败: %w", err)
	}
	return nil
}

// Get 读会话。
//
// 只有"key 不存在"算未找到;Redis 不可用必须作为错误上报 ——
// 前者是正常的"凭据无效",后者若被当成无效凭据会让用户在 Redis 抖动时
// 反复被要求输密码却始终失败,排查方向完全错。
func (s *RedisStore) Get(ctx context.Context, tokenHash string) (string, int64, bool, error) {
	if s.Cache == nil {
		return "", 0, false, ErrNoStore
	}
	k, err := s.key(tokenHash)
	if err != nil {
		return "", 0, false, err
	}
	val, ok, err := s.Cache.GetString(ctx, k)
	if err != nil {
		return "", 0, false, err
	}
	if !ok {
		return "", 0, false, nil
	}
	userID, tv, parsed := splitSessionValue(val)
	if !parsed {
		// 值被写坏(旧版本格式?人为改动?)——清掉坏值并显式报错,便于排查,
		// 而不是静默当作"凭据无效"让用户反复重输密码。
		_ = s.Cache.Del(ctx, k)
		return "", 0, false, fmt.Errorf("WebDAV 会话值格式非法")
	}
	return userID, tv, true, nil
}

// Delete 删除会话。
func (s *RedisStore) Delete(ctx context.Context, tokenHash string) error {
	if s.Cache == nil {
		return ErrNoStore
	}
	k, err := s.key(tokenHash)
	if err != nil {
		return err
	}
	return s.Cache.Del(ctx, k)
}

func splitSessionValue(v string) (string, int64, bool) {
	i := lastIndexByte(v, ':')
	if i <= 0 || i == len(v)-1 {
		return "", 0, false
	}
	tv, err := strconv.ParseInt(v[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return v[:i], tv, true
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
