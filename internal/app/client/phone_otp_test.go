package client

import (
	"context"
	"regexp"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

func TestNormalizePhone(t *testing.T) {
	t.Parallel()
	phone, err := normalizePhone("+8613812345678")
	require.NoError(t, err)
	require.Equal(t, "+8613812345678", phone)

	_, err = normalizePhone("123")
	require.Error(t, err)
}

func TestAccount_PhoneOTPLogin(t *testing.T) {
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

	cfg := &config.AppConfig{Messaging: &config.Messaging{DevLogSms: true}}
	projectRepo := bunrepo.NewProjectRepository(db)

	sms := &CaptureSMSSender{}
	account := NewTestAccountWithDeps(cfg, projectRepo, nil, db, rdb, nil, sms)

	challenge, err := account.CreatePhoneOTP(ctx, CreatePhoneOTPCommand{
		ProjectID: projectID,
		Phone:     "+8613900000001",
	})
	require.NoError(t, err)
	require.NotEmpty(t, challenge.ChallengeID)
	require.Len(t, sms.Body, 1)

	re := regexp.MustCompile(`code is: (\d{6})`)
	matches := re.FindStringSubmatch(sms.Body[0])
	require.Len(t, matches, 2)

	user, tokens, _, _, err := account.CreatePhoneOTPSession(ctx, CreatePhoneOTPSessionCommand{
		ProjectID:   projectID,
		Phone:       "+8613900000001",
		ChallengeID: challenge.ChallengeID,
		OTP:         matches[1],
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)
	require.Equal(t, "phone_8613900000001@torchwood.local", user.Email)
	require.NotEmpty(t, user.ID)
}
