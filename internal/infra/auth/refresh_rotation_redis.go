package auth

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GraceWindow 是上一轮 refresh token id 的宽限时长:窗口内以旧 id 刷新按当前
// 链正常续签(RotateGraceReuse,链不推进),不判重用、不连坐撤销。覆盖多标签
// 页并发刷新与刷新响应丢失后的重试;窗口外旧 id 仍判 RotateMismatch(真重放)。
const GraceWindow = time.Minute

// prevKeySuffix 是 previous 槽(宽限槽)的 key 后缀。
const prevKeySuffix = ":prev"

// refreshRotateScript 原子完成"读取当前 token id -> 比对 -> 一致则写入新 id"，
// 避免 GET-改-SET 竞态导致同一 refresh token 被并发轮换出多个新 token。
// presented 命中宽限槽(previous)时链不推进,返回 'grace|<current>' 由调用方
// 以 current 重签。返回值：ok / grace|<cur> / mismatch / missing。
const refreshRotateScript = `
local cur = redis.call('GET', KEYS[1])
if not cur then
  return 'missing'
end
if cur == ARGV[1] then
  redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[4])
  redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
  return 'ok'
end
local prev = redis.call('GET', KEYS[2])
if prev and prev == ARGV[1] then
  return 'grace|' .. cur
end
return 'mismatch'
`

// refreshRegisterScript 登记签发的 refresh token id。写入值 != 当前值 = 新链
// (sign-in/sign-up):清宽限槽,旧链 id 不得再续签;写入值 == 当前值 = 轮换
// 签发路径的幂等重写(Rotate 已原子写入),保留宽限槽,否则宽限语义失效。
const refreshRegisterScript = `
local cur = redis.call('GET', KEYS[1])
if cur and cur ~= ARGV[1] then
  redis.call('DEL', KEYS[2])
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
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
	if err := s.rdb.Eval(ctx, refreshRegisterScript, []string{key, key + prevKeySuffix},
		tokenID, ttl.Milliseconds()).Err(); err != nil {
		return status.Error(codes.Internal, "refresh rotation store failed")
	}
	return nil
}

func (s *RedisRefreshRotationStore) Rotate(ctx context.Context, key, presentedTokenID, newTokenID string, ttl time.Duration) (domainauth.RotateResult, string, error) {
	res, err := s.rdb.Eval(ctx, refreshRotateScript, []string{key, key + prevKeySuffix},
		presentedTokenID, newTokenID, ttl.Milliseconds(), GraceWindow.Milliseconds()).Text()
	if err != nil {
		return domainauth.RotateMissing, "", status.Error(codes.Internal, "refresh rotation store failed")
	}
	switch {
	case res == "ok":
		return domainauth.RotateOK, "", nil
	case strings.HasPrefix(res, "grace|"):
		return domainauth.RotateGraceReuse, strings.TrimPrefix(res, "grace|"), nil
	case res == "mismatch":
		return domainauth.RotateMismatch, "", nil
	default: // missing
		return domainauth.RotateMissing, "", nil
	}
}

var _ domainauth.RefreshRotationStore = (*RedisRefreshRotationStore)(nil)
