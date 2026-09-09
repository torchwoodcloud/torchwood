package auth

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// rateLimitWindowScript 原子执行 INCR + 首次 EXPIRE 并返回 [count, ttl]：
// 拒绝时 ttl 即当前窗口剩余秒数，供 RetryInfo 精确给出建议退避
// （Round4 J3-6）。与 incrWithTTL 的崩溃安全性一致：不存在「计数已增但
// TTL 未设」的中间态。
const rateLimitWindowScript = `
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return {count, redis.call('TTL', KEYS[1])}
`

// RedisRateLimiter 基于 Redis 固定窗口计数（INCR + EXPIRE）的通用限流器，
// 与 OTP/login throttle 的窗口实现保持一致。超限返回携带 google.rpc.RetryInfo
// detail 的 ResourceExhausted（RetryDelay=窗口剩余秒数）。
type RedisRateLimiter struct {
	rdb *redis.Client
}

func NewRedisRateLimiter(rdb *redis.Client) *RedisRateLimiter {
	return &RedisRateLimiter{rdb: rdb}
}

func (l *RedisRateLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) error {
	if key == "" {
		return nil
	}
	redisKey := "Torchwood:ratelimit:" + key
	res, err := l.rdb.Eval(ctx, rateLimitWindowScript, []string{redisKey}, int64(window.Seconds())).Slice()
	if err != nil {
		return status.Error(codes.Internal, "rate limit check failed")
	}
	count, _ := res[0].(int64)
	ttl, _ := res[1].(int64)
	if count > int64(limit) {
		return rateLimited(ttl)
	}
	return nil
}

// rateLimited 构造携带 RetryInfo + error_id 的 ResourceExhausted；ttl<=0
// （键已过期/无 TTL 的异常态）时退化为无 detail 的裸错误。error_id 与域码
// 体系对齐（redesign §4.1）：限流拒绝同样可引用、可归因。
func rateLimited(ttlSeconds int64) error {
	if ttlSeconds <= 0 {
		return rateLimitedStatus(0)
	}
	return rateLimitedStatus(ttlSeconds)
}

func rateLimitedStatus(ttlSeconds int64) error {
	st := status.New(codes.ResourceExhausted, "rate limit exceeded")
	st, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   "RATE_LIMIT.EXCEEDED",
		Domain:   "torchwood.platform",
		Metadata: map[string]string{"retryable": "true", "error_id": idgen.UUID().String()},
	})
	if err == nil && ttlSeconds > 0 {
		st, err = st.WithDetails(&errdetails.RetryInfo{
			RetryDelay: durationpb.New(time.Duration(ttlSeconds) * time.Second),
		})
	}
	if err != nil {
		return status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	return st.Err()
}

var _ domainauth.RateLimiter = (*RedisRateLimiter)(nil)
