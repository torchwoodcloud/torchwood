package client

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

func TestAccount_EmailOTPLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	cfg := buildTestConfig()
	cfgWithDevMailer := &config.AppConfig{
		Messaging: &config.Messaging{DevLogOtp: true},
	}
	_ = cfg

	projectRepo := bunrepo.NewProjectRepository(db)

	mailer := &CaptureMailer{}
	account := NewTestAccountWithMailer(cfgWithDevMailer, projectRepo, db, rdb, mailer)

	challenge, err := account.CreateEmailOTP(ctx, CreateEmailOTPCommand{
		ProjectID: projectID,
		Email:     "otp-user@torchwood.local",
	})
	require.NoError(t, err)
	require.NotEmpty(t, challenge.ChallengeID)
	require.Len(t, mailer.Bodies, 1)

	re := regexp.MustCompile(`code is: (\d{6})`)
	matches := re.FindStringSubmatch(mailer.Bodies[0])
	require.Len(t, matches, 2)
	code := matches[1]

	user, tokens, _, _, err := account.CreateEmailOTPSession(ctx, CreateEmailOTPSessionCommand{
		ProjectID:   projectID,
		Email:       "otp-user@torchwood.local",
		ChallengeID: challenge.ChallengeID,
		OTP:         code,
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, "otp-user@torchwood.local", user.Email)
	require.True(t, user.EmailVerified)
	require.NotEmpty(t, tokens.AccessToken)

	// Existing user path: second login with new challenge.
	// 跳过 60s 的发送冷却（otpSendCooldown），否则第二次发送会被限流。
	mr.FastForward(61 * time.Second)
	mailer.Bodies = nil
	challenge2, err := account.CreateEmailOTP(ctx, CreateEmailOTPCommand{
		ProjectID: projectID,
		Email:     "otp-user@torchwood.local",
	})
	require.NoError(t, err)
	matches = re.FindStringSubmatch(mailer.Bodies[0])
	require.Len(t, matches, 2)

	user2, tokens2, _, _, err := account.CreateEmailOTPSession(ctx, CreateEmailOTPSessionCommand{
		ProjectID:   projectID,
		Email:       "otp-user@torchwood.local",
		ChallengeID: challenge2.ChallengeID,
		OTP:         matches[1],
	})
	require.NoError(t, err)
	require.Equal(t, user.ID, user2.ID)
	require.NotEmpty(t, tokens2.AccessToken)
}
