package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// 默认阈值（T-01 裁决）：账号与 IP 双维各 5 次失败/60s 窗口，可经
// security.login_throttle 配置覆盖。超限时 429（ResourceExhausted）并
// 携带 RetryInfo detail（网关转译为 Retry-After 头）。
const (
	DefaultLoginEmailFailLimit  = 5
	DefaultLoginEmailFailWindow = time.Minute
	DefaultLoginIPFailLimit     = 5
	DefaultLoginIPFailWindow    = time.Minute
)

// LoginThrottleTuning 是登录频控的可配置参数（infra 内部传递形态）。
type LoginThrottleTuning struct {
	EmailLimit  int
	EmailWindow time.Duration
	IPLimit     int
	IPWindow    time.Duration
}

// DefaultLoginThrottleTuning 返回内置默认值。
func DefaultLoginThrottleTuning() LoginThrottleTuning {
	return LoginThrottleTuning{
		EmailLimit:  DefaultLoginEmailFailLimit,
		EmailWindow: DefaultLoginEmailFailWindow,
		IPLimit:     DefaultLoginIPFailLimit,
		IPWindow:    DefaultLoginIPFailWindow,
	}
}

// LoginThrottleTuningFromConfig 从应用配置解析频控参数：未配置/非法值回落
// 内置默认（与通用限流 resolveRateLimitDimension 同语义）。
func LoginThrottleTuningFromConfig(cfg *config.AppConfig) LoginThrottleTuning {
	t := DefaultLoginThrottleTuning()
	lt := cfg.GetSecurity().GetLoginThrottle()
	if d := lt.GetEmail(); d.GetLimit() > 0 {
		t.EmailLimit = int(d.GetLimit())
	}
	if w := parseThrottleWindow(lt.GetEmail().GetWindow()); w > 0 {
		t.EmailWindow = w
	}
	if d := lt.GetIp(); d.GetLimit() > 0 {
		t.IPLimit = int(d.GetLimit())
	}
	if w := parseThrottleWindow(lt.GetIp().GetWindow()); w > 0 {
		t.IPWindow = w
	}
	return t
}

func parseThrottleWindow(s string) time.Duration {
	if s == "" {
		return 0
	}
	w, err := time.ParseDuration(s)
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

// RedisLoginThrottle throttles password sign-in attempts using sliding-window
// failure counters in Redis.
type RedisLoginThrottle struct {
	rdb    *redis.Client
	tuning LoginThrottleTuning
}

// NewRedisLoginThrottle constructs the throttle with T-01 defaults; use
// NewRedisLoginThrottleWithTuning for config-driven limits.
func NewRedisLoginThrottle(rdb *redis.Client) *RedisLoginThrottle {
	return NewRedisLoginThrottleWithTuning(rdb, DefaultLoginThrottleTuning())
}

func NewRedisLoginThrottleWithTuning(rdb *redis.Client, tuning LoginThrottleTuning) *RedisLoginThrottle {
	return &RedisLoginThrottle{rdb: rdb, tuning: tuning}
}

// NewRedisLoginThrottleFromConfig 从应用配置装配登录频控（T-01：阈值经
// security.login_throttle 可配置，未配置回落内置默认）。
func NewRedisLoginThrottleFromConfig(rdb *redis.Client, cfg *config.AppConfig) *RedisLoginThrottle {
	return NewRedisLoginThrottleWithTuning(rdb, LoginThrottleTuningFromConfig(cfg))
}

func (s *RedisLoginThrottle) Check(ctx context.Context, namespace, email, ip string) error {
	if limited, err := s.overLimit(ctx, s.key(namespace, "email", email), int64(s.tuning.EmailLimit)); err != nil {
		return err
	} else if limited {
		return throttleExhausted(s.tuning.EmailWindow)
	}
	if limited, err := s.overLimit(ctx, s.key(namespace, "ip", ip), int64(s.tuning.IPLimit)); err != nil {
		return err
	} else if limited {
		return throttleExhausted(s.tuning.IPWindow)
	}
	return nil
}

func (s *RedisLoginThrottle) RecordFailure(ctx context.Context, namespace, email, ip string, recordEmail bool) error {
	// recordEmail=false：未注册邮箱的失败只计 IP 维度（T-01），邮箱键永不
	// 落笔——既保留 R05-P1-5 的防"锁死任意邮箱"语义，也让 IP 维度与账号
	// 存在性解耦。
	if recordEmail {
		if err := s.incr(ctx, s.key(namespace, "email", email), s.tuning.EmailWindow); err != nil {
			return err
		}
	}
	return s.incr(ctx, s.key(namespace, "ip", ip), s.tuning.IPWindow)
}

func (s *RedisLoginThrottle) Reset(ctx context.Context, namespace, email, ip string) error {
	return s.rdb.Del(ctx, s.key(namespace, "email", email), s.key(namespace, "ip", ip)).Err()
}

func (s *RedisLoginThrottle) overLimit(ctx context.Context, key string, max int64) (bool, error) {
	if key == "" {
		return false, nil
	}
	count, err := s.rdb.Get(ctx, key).Int64()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, status.Error(codes.Internal, "login throttle check failed")
	}
	return count >= max, nil
}

func (s *RedisLoginThrottle) incr(ctx context.Context, key string, window time.Duration) error {
	if key == "" {
		return nil
	}
	// Round3 H6-4：INCR + 首次 EXPIRE 原子化，崩溃不留下无 TTL 计数键。
	if _, err := incrWithTTL(ctx, s.rdb, key, window); err != nil {
		return status.Error(codes.Internal, "login throttle update failed")
	}
	return nil
}

// key 为空值维度返回空串（overLimit/incr 均按空串跳过），不产生 "email:"
// 这类残缺键。
func (s *RedisLoginThrottle) key(namespace, dimension, value string) string {
	if value == "" {
		return ""
	}
	return fmt.Sprintf("Torchwood:login:fail:%s:%s:%s", namespace, dimension, value)
}

// throttleExhausted 构造带 RetryInfo detail 的 429：RetryDelay 取整窗口作为
// 保守退避估计（与通用限流拦截器 withRetryInfoFallback 同约定），网关侧
// HTTPErrorHandler 转译为 Retry-After 响应头。两条失败路径（邮箱维度/IP
// 维度）返回同一文案，不泄露触发维度与账号存在性。
func throttleExhausted(window time.Duration) error {
	st := status.New(codes.ResourceExhausted, "too many failed sign-in attempts, try again later")
	if window > 0 {
		if enriched, err := st.WithDetails(&errdetails.RetryInfo{
			RetryDelay: durationpb.New(window),
		}); err == nil {
			return enriched.Err()
		}
	}
	return st.Err()
}

var _ domainauth.LoginThrottle = (*RedisLoginThrottle)(nil)
