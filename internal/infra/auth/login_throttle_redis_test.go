package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestLoginThrottle(t *testing.T) (*miniredis.Miniredis, *auth.RedisLoginThrottle, context.Context) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, auth.NewRedisLoginThrottle(rdb), context.Background()
}

// TestRedisLoginThrottle_TwoDimensionBudgets（T-01 验收：双维独立计数，
// 默认 5 次/60s）：第 5 次失败后，同账号（任意 IP）与同 IP（任意账号）
// 均触发 429；换账号不放行 IP 维度，换 IP 不放行账号维度。
func TestRedisLoginThrottle_TwoDimensionBudgets(t *testing.T) {
	t.Parallel()
	mr, throttle, ctx := newTestLoginThrottle(t)

	const email, ip = "user@torchwood.local", "203.0.113.1"
	for i := 0; i < 5; i++ {
		require.NoError(t, throttle.Check(ctx, "end_user", email, ip))
		require.NoError(t, throttle.RecordFailure(ctx, "end_user", email, ip, true))
	}

	// 第 6 次：Check 先行拒绝（429）。
	err := throttle.Check(ctx, "end_user", email, ip)
	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	// 换 IP 不放行账号维度。
	require.Equal(t, codes.ResourceExhausted, status.Code(throttle.Check(ctx, "end_user", email, "198.51.100.9")))
	// 换账号不放行 IP 维度。
	require.Equal(t, codes.ResourceExhausted, status.Code(throttle.Check(ctx, "end_user", "other@torchwood.local", ip)))
	// 都换 → 放行。
	require.NoError(t, throttle.Check(ctx, "end_user", "other@torchwood.local", "198.51.100.9"))

	// 窗口过期后恢复。
	mr.FastForward(time.Minute + time.Second)
	require.NoError(t, throttle.Check(ctx, "end_user", email, ip))
}

// TestRedisLoginThrottle_NamespaceIsolation 与 Reset（登录成功计数归零）。
func TestRedisLoginThrottle_NamespaceIsolationAndReset(t *testing.T) {
	t.Parallel()
	_, throttle, ctx := newTestLoginThrottle(t)

	const email = "reset@torchwood.local"
	for i := 0; i < 5; i++ {
		require.NoError(t, throttle.RecordFailure(ctx, "end_user", email, "203.0.113.2", true))
	}
	// 同邮箱不同 namespace（admin 面）不受影响。
	require.NoError(t, throttle.Check(ctx, "admin", email, ""))

	require.Equal(t, codes.ResourceExhausted, status.Code(throttle.Check(ctx, "end_user", email, "")))
	require.NoError(t, throttle.Reset(ctx, "end_user", email, "203.0.113.2"))
	require.NoError(t, throttle.Check(ctx, "end_user", email, ""))
}

// TestRedisLoginThrottle_IPOnlyRecording（T-01 裁决：未注册邮箱失败只计 IP
// 维度）：邮箱键永不落笔，IP 计数照常累加并可独立触发 429——IP 维度与
// 账号存在性解耦。
func TestRedisLoginThrottle_IPOnlyRecording(t *testing.T) {
	t.Parallel()
	mr, throttle, ctx := newTestLoginThrottle(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	const email, ip = "ghost@torchwood.local", "203.0.113.3"
	for i := 0; i < 5; i++ {
		require.NoError(t, throttle.RecordFailure(ctx, "end_user", email, ip, false))
	}
	// 邮箱键不存在。
	n, err := rdb.Exists(ctx, "Torchwood:login:fail:end_user:email:"+email).Result()
	require.NoError(t, err)
	require.Zero(t, n, "未注册邮箱不得写入邮箱键")
	// IP 维度照常触发 429。
	require.Equal(t, codes.ResourceExhausted, status.Code(throttle.Check(ctx, "end_user", email, ip)))
}

// TestRedisLoginThrottle_RetryInfoDetail：429 携带 RetryInfo detail（窗口
// 剩余的保守估计 = 整窗口），网关转译为 Retry-After。
func TestRedisLoginThrottle_RetryInfoDetail(t *testing.T) {
	t.Parallel()
	_, throttle, ctx := newTestLoginThrottle(t)

	const email, ip = "retry@torchwood.local", "203.0.113.4"
	for i := 0; i < 5; i++ {
		require.NoError(t, throttle.RecordFailure(ctx, "end_user", email, ip, true))
	}
	err := throttle.Check(ctx, "end_user", email, ip)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, st.Code())
	var found *errdetails.RetryInfo
	for _, d := range st.Details() {
		if ri, isRetry := d.(*errdetails.RetryInfo); isRetry {
			found = ri
		}
	}
	require.NotNil(t, found)
	require.Equal(t, time.Minute, found.GetRetryDelay().AsDuration())
}

// TestRedisLoginThrottle_ConfigTuning：配置覆盖默认阈值；文案对两条触发
// 路径一致（不泄露维度）。
func TestRedisLoginThrottle_ConfigTuning(t *testing.T) {
	t.Parallel()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	cfg := &config.AppConfig{Security: &config.Security{
		LoginThrottle: &config.Security_LoginThrottle{
			Email: &config.Security_RateLimit_Dimension{Limit: 2, Window: "1h"},
			Ip:    &config.Security_RateLimit_Dimension{Limit: 9, Window: "30s"},
		},
	}}
	throttle := auth.NewRedisLoginThrottleFromConfig(rdb, cfg)

	const email, ip = "tuned@torchwood.local", "203.0.113.5"
	require.NoError(t, throttle.RecordFailure(ctx, "end_user", email, ip, true))
	require.NoError(t, throttle.RecordFailure(ctx, "end_user", email, ip, true))
	// 邮箱维度 2 次即触发。
	errEmail := throttle.Check(ctx, "end_user", email, "198.51.100.1")
	require.Equal(t, codes.ResourceExhausted, status.Code(errEmail))
	require.Equal(t, "too many failed sign-in attempts, try again later", status.Convert(errEmail).Message())
	// IP 维度 9 次才触发（未到阈值前同 IP 换账号放行）。
	require.NoError(t, throttle.Check(ctx, "end_user", "x@torchwood.local", ip))
}

// TestRedisLoginThrottle_EmptyDimensions：空邮箱/空 IP 不产生残缺键，也
// 不触发任何维度（维度缺失时由通用 IP 限流兜底）。
func TestRedisLoginThrottle_EmptyDimensions(t *testing.T) {
	t.Parallel()
	mr, throttle, ctx := newTestLoginThrottle(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	require.NoError(t, throttle.RecordFailure(ctx, "end_user", "", "203.0.113.6", true))
	require.NoError(t, throttle.RecordFailure(ctx, "end_user", "a@b.c", "", true))
	n, err := rdb.Exists(ctx, "Torchwood:login:fail:end_user:email:", "Torchwood:login:fail:end_user:ip:").Result()
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, throttle.Check(ctx, "end_user", "", ""))
}
