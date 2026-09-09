package auth

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// refreshRotateScript 原子完成"读取当前 token id -> 比对 -> 一致则写入新 id"，
// 避免 GET-改-SET 竞态导致同一 refresh token 被并发轮换出多个新 token。
// 返回值：ok / mismatch / missing。
const refreshRotateScript = `
local cur = redis.call('GET', KEYS[1])
if not cur then
  return 'missing'
end
if cur ~= ARGV[1] then
  return 'mismatch'
end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 'ok'
`

// RedisRefreshRotationStore stores the current refresh token ids in Redis.
type RedisRefreshRotationStore struct {
	rdb *redis.Client
}

func NewRedisRefreshRotationStore(rdb *redis.Client) *RedisRefreshRotationStore {
	return &RedisRefreshRotationStore{rdb: rdb}
}

func (s *RedisRefreshRotationStore) Register(ctx context.Context, key, tokenID string, ttl time.Duration) error {
	if key == "" || tokenID == "" {
		return nil
	}
	if err := s.rdb.Set(ctx, key, tokenID, ttl).Err(); err != nil {
		return status.Error(codes.Internal, "refresh rotation store failed")
	}
	return nil
}

func (s *RedisRefreshRotationStore) Rotate(ctx context.Context, key, presentedTokenID, newTokenID string, ttl time.Duration) (domainauth.RotateResult, error) {
	result, err := s.rdb.Eval(ctx, refreshRotateScript, []string{key},
		presentedTokenID, newTokenID, ttl.Milliseconds()).Text()
	if err != nil {
		return domainauth.RotateMissing, status.Error(codes.Internal, "refresh rotation store failed")
	}
	switch result {
	case "ok":
		return domainauth.RotateOK, nil
	case "mismatch":
		return domainauth.RotateMismatch, nil
	default: // missing
		return domainauth.RotateMissing, nil
	}
}

var _ domainauth.RefreshRotationStore = (*RedisRefreshRotationStore)(nil)
