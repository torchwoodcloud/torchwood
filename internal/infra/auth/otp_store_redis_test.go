package auth_test

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newOTPTestConfig(secret string) *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: secret},
		},
	}
}

func newOTPTestStore(t *testing.T, secret string) (*auth.RedisOTPChallengeStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return auth.NewRedisOTPChallengeStore(rdb, newOTPTestConfig(secret)), mr
}

func requireGRPCCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "expected grpc status error, got %v", err)
	require.Equal(t, code, st.Code())
}

// M5 C8：OTP verify 公开消费口按 IP 频控（对齐 account token 的 30 次/
// 15min，共用计数桶）；IP 缺省的上下文不受影响（fail-open 由传输层保证
// ClientInfo 恒注入兜底）。
func TestRedisOTPChallengeStore_VerifyIPLimited(t *testing.T) {
	t.Parallel()

	store, _ := newOTPTestStore(t, "otp-test-secret")
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "198.51.100.7"})

	for i := 0; i < 30; i++ {
		err := store.VerifyEmailChallenge(ctx, "proj1", "ch", "user@example.com", "000000")
		requireGRPCCode(t, err, codes.Unauthenticated)
	}
	err := store.VerifyEmailChallenge(ctx, "proj1", "ch", "user@example.com", "000000")
	requireGRPCCode(t, err, codes.ResourceExhausted)

	// 不同 IP 独立计数。
	otherIP := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "198.51.100.8"})
	err = store.VerifyEmailChallenge(otherIP, "proj1", "ch", "user@example.com", "000000")
	requireGRPCCode(t, err, codes.Unauthenticated)
}

func TestRedisOTPChallengeStore_VerifyFlow(t *testing.T) {
	t.Parallel()

	store, _ := newOTPTestStore(t, "otp-test-secret")
	ctx := context.Background()

	require.NoError(t, store.CheckSendRateLimit(ctx, "proj1", "user@example.com", "1.2.3.4"))

	code := "123456"
	challengeID, _, err := store.CreateEmailChallenge(ctx, "proj1", "user@example.com", code)
	require.NoError(t, err)
	require.NotEmpty(t, challengeID)

	err = store.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", "000000")
	requireGRPCCode(t, err, codes.Unauthenticated)

	require.NoError(t, store.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", code))

	phoneChallengeID, _, err := store.CreatePhoneChallenge(ctx, "proj1", "+8613812345678", "654321")
	require.NoError(t, err)
	require.NoError(t, store.VerifyPhoneChallenge(ctx, "proj1", phoneChallengeID, "+8613812345678", "654321"))

	// 重放：同一正确验证码第二次校验必须失败。
	err = store.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", code)
	requireGRPCCode(t, err, codes.Unauthenticated)
}

func TestRedisOTPChallengeStore_ConcurrentVerifySingleSuccess(t *testing.T) {
	t.Parallel()

	store, _ := newOTPTestStore(t, "otp-test-secret")
	ctx := context.Background()

	code := "123456"
	challengeID, _, err := store.CreateEmailChallenge(ctx, "proj1", "user@example.com", code)
	require.NoError(t, err)

	// 并发用同一正确验证码校验，原子语义下只能有一个成功。
	const workers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", code); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, successes, "exactly one concurrent verify may succeed")
}

func TestRedisOTPChallengeStore_AttemptsLockout(t *testing.T) {
	t.Parallel()

	store, _ := newOTPTestStore(t, "otp-test-secret")
	ctx := context.Background()

	challengeID, _, err := store.CreateEmailChallenge(ctx, "proj1", "user@example.com", "123456")
	require.NoError(t, err)

	// 错误验证码递增尝试次数，5 次内均为 Unauthenticated。
	for i := 0; i < 5; i++ {
		err := store.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", "000000")
		requireGRPCCode(t, err, codes.Unauthenticated)
	}
	// 超限后锁定，返回 ResourceExhausted。
	err = store.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", "000000")
	requireGRPCCode(t, err, codes.ResourceExhausted)
	// 锁定后 challenge 已删除，正确验证码也无法再通过。
	err = store.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", "123456")
	requireGRPCCode(t, err, codes.Unauthenticated)
}

func TestRedisOTPChallengeStore_HMACKeyIsolation(t *testing.T) {
	t.Parallel()

	// 同一 Redis、不同密钥的两个 store：验证码哈希互不可校验。
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	creator := auth.NewRedisOTPChallengeStore(rdb, newOTPTestConfig("secret-a"))
	otherKey := auth.NewRedisOTPChallengeStore(rdb, newOTPTestConfig("secret-b"))
	ctx := context.Background()

	code := "123456"
	challengeID, _, err := creator.CreateEmailChallenge(ctx, "proj1", "user@example.com", code)
	require.NoError(t, err)

	// 密钥不同则 HMAC 结果不同，正确验证码也校验失败（消耗一次尝试次数）。
	err = otherKey.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", code)
	requireGRPCCode(t, err, codes.Unauthenticated)
	require.NoError(t, creator.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", code))
}

func TestRedisOTPChallengeStore_ExpiredTTLDoesNotPersist(t *testing.T) {
	t.Parallel()

	store, mr := newOTPTestStore(t, "otp-test-secret")
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	challengeID, _, err := store.CreateEmailChallenge(ctx, "proj1", "user@example.com", "123456")
	require.NoError(t, err)
	key := "Torchwood:otp:ch:" + challengeID
	require.NoError(t, rdb.Persist(ctx, key).Err())

	err = store.VerifyEmailChallenge(ctx, "proj1", challengeID, "user@example.com", "000000")
	requireGRPCCode(t, err, codes.Unauthenticated)
	n, err := rdb.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, n, "PTTL<=0 时不得把 challenge 写成永不过期键")
}

func TestRedisOTPChallengeStore_SendCooldown(t *testing.T) {
	t.Parallel()

	store, _ := newOTPTestStore(t, "otp-test-secret")
	ctx := context.Background()

	require.NoError(t, store.CheckSendRateLimit(ctx, "proj1", "user@example.com", ""))
	err := store.CheckSendRateLimit(ctx, "proj1", "user@example.com", "")
	requireGRPCCode(t, err, codes.ResourceExhausted)
}

func TestGenerateOTP(t *testing.T) {
	t.Parallel()
	code, err := auth.GenerateOTP(6)
	require.NoError(t, err)
	require.Len(t, code, 6)
}

func TestHashOTP(t *testing.T) {
	t.Parallel()
	// HashOTP 仅用于高熵 secret（account token），保持确定性哈希行为。
	require.Equal(t, auth.HashOTP("secret"), auth.HashOTP("secret"))
	require.NotEqual(t, auth.HashOTP("a"), auth.HashOTP("b"))
}
