package auth_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"github.com/torchwoodcloud/torchwood/pkg/secretbox"
)

func totpTestConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "test-mfa-jwt-secret"}, // #nosec G101 -- 测试固定值
		},
	}
}

func TestTOTPService_CreateAndVerify(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := auth.NewTOTPService(totpTestConfig(), rdb)
	ctx := context.Background()

	factor, plainSecret, otpauthURL, err := svc.CreateTOTPFactor(ctx, "Torchwood Test", "user-1", "mfa@example.com")
	require.NoError(t, err)
	require.NotEmpty(t, factor.ID)
	require.Equal(t, "totp", factor.Type)
	require.Equal(t, "pending", factor.Status)
	require.NotEmpty(t, plainSecret)
	require.Contains(t, otpauthURL, "otpauth://totp/")
	require.Contains(t, otpauthURL, "issuer=Torchwood%20Test")
	require.Contains(t, otpauthURL, "secret=")
	// 落库的是密文，不是明文。
	require.NotContains(t, factor.Secret, plainSecret)
	require.Contains(t, factor.Secret, "enc:v1:")

	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, svc.VerifyTOTPFactor(ctx, factor, code))
}

func TestTOTPService_ReplayRejected(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := auth.NewTOTPService(totpTestConfig(), rdb)
	ctx := context.Background()

	factor, plainSecret, _, err := svc.CreateTOTPFactor(ctx, "Issuer", "user-1", "mfa@example.com")
	require.NoError(t, err)

	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, svc.VerifyTOTPFactor(ctx, factor, code))
	// 同一 code 60s 内重放被拒绝（含上一窗口 step 的重放）。
	err = svc.VerifyTOTPFactor(ctx, factor, code)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already used")
}

func TestTOTPService_ValidateReplayRejected(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := auth.NewTOTPService(totpTestConfig(), rdb)
	ctx := context.Background()

	factor, plainSecret, _, err := svc.CreateTOTPFactor(ctx, "Issuer", "user-1", "mfa@example.com")
	require.NoError(t, err)
	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, svc.ValidateTOTP(ctx, factor, code))
	// ValidateTOTP 现在同样做 60s 防重放：同一 code 二次校验被拒绝
	//（登录挑战此前仅靠 challenge token 一次性防重放）。
	err = svc.ValidateTOTP(ctx, factor, code)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already used")
}

func TestTOTPService_ValidateInvalidCodeLockout(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := auth.NewTOTPService(totpTestConfig(), rdb)
	ctx := context.Background()

	factor, plainSecret, _, err := svc.CreateTOTPFactor(ctx, "Issuer", "user-1", "mfa@example.com")
	require.NoError(t, err)

	// 连续 5 次错误 code 后锁定：即使正确 code 也拒绝。
	for i := 0; i < 5; i++ {
		err := svc.ValidateTOTP(ctx, factor, "000000")
		require.Error(t, err)
	}
	err = svc.ValidateTOTP(ctx, factor, "000000")
	require.Error(t, err)
	require.Contains(t, err.Error(), "attempts exceeded")

	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	err = svc.ValidateTOTP(ctx, factor, code)
	require.Error(t, err)
	require.Contains(t, err.Error(), "attempts exceeded")
}

func TestTOTPService_InvalidCodeLockout(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := auth.NewTOTPService(totpTestConfig(), rdb)
	ctx := context.Background()

	factor, plainSecret, _, err := svc.CreateTOTPFactor(ctx, "Issuer", "user-1", "mfa@example.com")
	require.NoError(t, err)

	// 连续 5 次错误 code 后锁定：第 6 次（即使正确 code）也拒绝。
	for i := 0; i < 5; i++ {
		err := svc.VerifyTOTPFactor(ctx, factor, "000000")
		require.Error(t, err)
	}
	err = svc.VerifyTOTPFactor(ctx, factor, "000000")
	require.Error(t, err)
	require.Contains(t, err.Error(), "attempts exceeded")

	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	err = svc.VerifyTOTPFactor(ctx, factor, code)
	require.Error(t, err)
	require.Contains(t, err.Error(), "attempts exceeded")
}

func TestTOTPService_LockoutResetsOnSuccess(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := auth.NewTOTPService(totpTestConfig(), rdb)
	ctx := context.Background()

	factor, plainSecret, _, err := svc.CreateTOTPFactor(ctx, "Issuer", "user-1", "mfa@example.com")
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		require.Error(t, svc.VerifyTOTPFactor(ctx, factor, "000000"))
	}
	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, svc.VerifyTOTPFactor(ctx, factor, code))
	// 成功后计数清零；等防重放窗口（60s）过后新窗口 code 不再被锁定。
	mr.FastForward(61 * time.Second)
	code2, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, svc.ValidateTOTP(ctx, factor, code2))
}

func TestTOTPService_RequiresJWTSecret(t *testing.T) {
	svc := auth.NewTOTPService(&config.AppConfig{}, nil)
	ctx := context.Background()
	_, _, _, err := svc.CreateTOTPFactor(ctx, "Issuer", "user-1", "mfa@example.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not configured")
}

func TestTOTPService_LegacyMasterKeyFallback(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cfg := totpTestConfig()
	svc := auth.NewTOTPService(cfg, rdb)
	ctx := context.Background()

	master := cfg.GetSecurity().GetJwt().GetSecret()
	totpDomainKey := hex.EncodeToString(jwtparser.DeriveKey(master, "totp"))

	factor, plainSecret, _, err := svc.CreateTOTPFactor(ctx, "Issuer", "user-1", "mfa@example.com")
	require.NoError(t, err)

	// 域分离：新因子密文不能用主密钥解密。
	_, err = secretbox.Decrypt(factor.Secret, master)
	require.Error(t, err)

	// 模拟存量数据：用旧主密钥加密的 secret（双密钥读兼容）。
	legacy, err := secretbox.Encrypt(plainSecret, master)
	require.NoError(t, err)
	require.NotEqual(t, legacy, factor.Secret)
	factor.Secret = legacy

	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, svc.VerifyTOTPFactor(ctx, factor, code))
	// 校验成功后重加密迁移到 TOTP 域密钥（写新）。
	require.NotEqual(t, legacy, factor.Secret)
	rePlain, err := secretbox.Decrypt(factor.Secret, totpDomainKey)
	require.NoError(t, err)
	require.Equal(t, plainSecret, rePlain)

	// ValidateTOTP（登录路径）同样兼容存量主密钥密文（只读，不迁移）。
	factor2, _, _, err := svc.CreateTOTPFactor(ctx, "Issuer", "user-1", "mfa@example.com")
	require.NoError(t, err)
	factor2.Secret = legacy
	mr.FastForward(61 * time.Second)
	code2, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, svc.ValidateTOTP(ctx, factor2, code2))
}
