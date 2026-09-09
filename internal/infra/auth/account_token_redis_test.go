package auth_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
)

func TestRedisAccountTokenStore_Verification(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := auth.NewRedisAccountTokenStore(rdb)
	ctx := context.Background()

	secret, _, err := store.CreateVerificationToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)
	require.NotEmpty(t, secret)

	require.NoError(t, store.VerifyVerificationToken(ctx, "proj-1", "user-1", secret))
	require.Error(t, store.VerifyVerificationToken(ctx, "proj-1", "user-1", secret))
}

func TestRedisAccountTokenStore_MagicURL(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := auth.NewRedisAccountTokenStore(rdb)
	ctx := context.Background()

	challengeID, secret, expireAt, err := store.CreateMagicURLToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)
	require.NotEmpty(t, challengeID)
	require.NotEmpty(t, secret)
	require.NotEqual(t, challengeID, secret)
	require.False(t, expireAt.IsZero())

	// 一次性消费：成功后再次验证失败。
	require.NoError(t, store.VerifyMagicURLToken(ctx, "proj-1", "user-1", secret))
	require.Error(t, store.VerifyMagicURLToken(ctx, "proj-1", "user-1", secret))

	// 错误 secret 拒绝（Round3 H6-3：只计数不删除，正确 secret 仍可用）。
	challengeID2, secret2, _, err := store.CreateMagicURLToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)
	require.NotEmpty(t, challengeID2)
	require.Error(t, store.VerifyMagicURLToken(ctx, "proj-1", "user-1", secret2+"0"))
	require.NoError(t, store.VerifyMagicURLToken(ctx, "proj-1", "user-1", secret2), "错 secret 不得删除记录，正确 secret 仍可消费")
}

func TestRedisAccountTokenStore_MagicURLConcurrentConsume(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := auth.NewRedisAccountTokenStore(rdb)
	ctx := context.Background()

	_, secret, _, err := store.CreateMagicURLToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)

	// 并发双消费：GETDEL 原子性保证恰好一次成功。
	const attempts = 8
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = store.VerifyMagicURLToken(ctx, "proj-1", "user-1", secret)
		}(i)
	}
	wg.Wait()
	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	require.Equal(t, 1, successes)
}

func TestRedisAccountTokenStore_MagicURLExpiry(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := auth.NewRedisAccountTokenStore(rdb)
	ctx := context.Background()

	_, secret, _, err := store.CreateMagicURLToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)
	mr.FastForward(61 * time.Minute) // > 1h TTL
	require.Error(t, store.VerifyMagicURLToken(ctx, "proj-1", "user-1", secret))

	// purpose 隔离：recovery 的 token 不能用于 magic_url，反之亦然。
	recoverySecret, _, err := store.CreateRecoveryToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)
	require.Error(t, store.VerifyMagicURLToken(ctx, "proj-1", "user-1", recoverySecret))
}

// TestRedisAccountTokenStore_EmailChange（R05-P1-2 A 档）：创建/消费返回新
// 邮箱 + 一次性消费 + purpose 隔离（email_change 的 token 不能当 verification 用）。
func TestRedisAccountTokenStore_EmailChange(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := auth.NewRedisAccountTokenStore(rdb)
	ctx := context.Background()

	secret, expireAt, err := store.CreateEmailChangeToken(ctx, "proj-1", "user-1", "new-mail@example.com")
	require.NoError(t, err)
	require.NotEmpty(t, secret)
	require.False(t, expireAt.IsZero())

	// 消费返回 record 中的新邮箱。
	email, err := store.VerifyEmailChangeToken(ctx, "proj-1", "user-1", secret)
	require.NoError(t, err)
	require.Equal(t, "new-mail@example.com", email)

	// 一次性消费：二次使用拒绝。
	_, err = store.VerifyEmailChangeToken(ctx, "proj-1", "user-1", secret)
	require.Error(t, err)

	// purpose 隔离：email_change token 不能当 verification 用，反之亦然。
	secret2, _, err := store.CreateEmailChangeToken(ctx, "proj-1", "user-1", "another@example.com")
	require.NoError(t, err)
	require.Error(t, store.VerifyVerificationToken(ctx, "proj-1", "user-1", secret2))

	verificationSecret, _, err := store.CreateVerificationToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)
	_, err = store.VerifyEmailChangeToken(ctx, "proj-1", "user-1", verificationSecret)
	require.Error(t, err)

	// 错误 secret 拒绝（Round3 H6-3：只计数不删除，正确 secret 仍可消费）。
	secret3, _, _ := store.CreateEmailChangeToken(ctx, "proj-1", "user-1", "wrong@example.com")
	_, err = store.VerifyEmailChangeToken(ctx, "proj-1", "user-1", secret3+"0")
	require.Error(t, err)
	gotEmail, err := store.VerifyEmailChangeToken(ctx, "proj-1", "user-1", secret3)
	require.NoError(t, err, "错 secret 不得删除记录，正确 secret 仍可消费")
	require.Equal(t, "wrong@example.com", gotEmail)
}

// Round3 H6-3：错 secret 只计数（不删除记录、不重置 TTL），超过
// accountTokenMaxAttempts 次后删除并锁定（此后正确 secret 也 Unauthenticated）。
func TestRedisAccountTokenStore_WrongSecretCountsAndLocks(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := auth.NewRedisAccountTokenStore(rdb)
	ctx := context.Background()

	secret, _, err := store.CreateRecoveryToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)

	// 前 N-1 次错 secret：统一 Unauthenticated，记录仍在（正确 secret 可用）。
	for i := 0; i < auth.AccountTokenMaxAttempts-1; i++ {
		require.Error(t, store.VerifyRecoveryToken(ctx, "proj-1", "user-1", secret+"0"),
			"错 secret 必须拒绝（统一 Unauthenticated 防枚举）")
	}
	require.NoError(t, store.VerifyRecoveryToken(ctx, "proj-1", "user-1", secret),
		"未超限前正确 secret 仍可消费，记录未被删除")

	// 再创建一条，耗尽全部尝试次数 → 锁定：记录删除，正确 secret 也无法使用。
	secret2, _, err := store.CreateRecoveryToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)
	for i := 0; i < auth.AccountTokenMaxAttempts; i++ {
		require.Error(t, store.VerifyRecoveryToken(ctx, "proj-1", "user-1", secret2+"0"))
	}
	require.Error(t, store.VerifyRecoveryToken(ctx, "proj-1", "user-1", secret2),
		"超过尝试上限后记录已删除锁定，正确 secret 也拒绝")
}

// Round3 H6-3：错 secret 计数不重置 TTL——记录仍按原窗口过期。
func TestRedisAccountTokenStore_WrongSecretKeepsTTL(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := auth.NewRedisAccountTokenStore(rdb)
	ctx := context.Background()

	secret, _, err := store.CreateVerificationToken(ctx, "proj-1", "user-1", "user@example.com")
	require.NoError(t, err)
	require.Error(t, store.VerifyVerificationToken(ctx, "proj-1", "user-1", secret+"0"))

	mr.FastForward(25 * time.Hour) // > 24h TTL
	require.Error(t, store.VerifyVerificationToken(ctx, "proj-1", "user-1", secret),
		"错 secret 计数后记录仍按原 TTL 过期")
}
