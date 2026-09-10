package client

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type magicURLFixture struct {
	ctx       context.Context
	account   *Account
	projectID string
	userID    string
	mailer    *CaptureMailer
	mr        *miniredis.Miniredis
}

func setupMagicURL(t *testing.T, withMailer bool) *magicURLFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	// 配置 redirect 白名单，允许测试回调地址。
	require.NoError(t, updateProjectSettings(ctx, db, projectID, map[string]any{
		"auth.oauth_allowed_redirect_urls": []any{"https://app.example.com"},
	}))

	projectRepo := bunrepo.NewProjectRepository(db)

	var account *Account
	mailer := &CaptureMailer{}
	if withMailer {
		account = NewTestAccountWithMailer(mfaTestConfig(), projectRepo, db, rdb, mailer)
	} else {
		account = NewTestAccountWithRedis(mfaTestConfig(), projectRepo, db, rdb)
	}

	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "magic-user@example.com",
		Password:  "User@123",
		Name:      "Magic User",
	})
	require.NoError(t, err)
	_ = user

	return &magicURLFixture{
		ctx:       ctx,
		account:   account,
		projectID: projectID,
		userID:    user.ID,
		mailer:    mailer,
		mr:        mr,
	}
}

func (f *magicURLFixture) userContext() context.Context {
	return contexts.WithPrincipal(f.ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: f.projectID,
		UserID:    f.userID,
		Email:     "magic-user@example.com",
		Roles:     []string{"users", "user:" + f.userID},
	})
}

// linkSecret 从最新一封邮件正文的登录链接中提取真实 secret。
func (f *magicURLFixture) linkSecret(t *testing.T) string {
	t.Helper()
	require.NotEmpty(t, f.mailer.Bodies)
	re := regexp.MustCompile(`secret=([a-f0-9]+)`)
	m := re.FindStringSubmatch(f.mailer.Bodies[len(f.mailer.Bodies)-1])
	require.NotNil(t, m, "mail body should contain a secret link")
	return m[1]
}

// updateProjectSettings 直接更新项目 settings（测试用，绕过 Server API）。
func updateProjectSettings(ctx context.Context, db *clients.Database, projectID string, settings map[string]any) error {
	_, err := db.NewUpdate().Model((*model.Project)(nil)).
		Set("settings = ?", settings).
		Where("id = ?", projectID).
		Exec(ctx)
	return err
}

func TestAccount_CreateMagicURLSession_SendsLink(t *testing.T) {
	f := setupMagicURL(t, true)

	challenge, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)
	require.NotEmpty(t, challenge.ChallengeID)
	require.False(t, challenge.ExpireAt.IsZero())
	require.Len(t, f.mailer.Bodies, 1)
	re := regexp.MustCompile(`https://app\.example\.com/signin\?secret=[a-f0-9]+&userId=[0-9a-f-]+`)
	require.Regexp(t, re, f.mailer.Bodies[0])
	// 响应只回传不透明 challengeID，不回传可登录的 secret。
	require.NotEqual(t, f.linkSecret(t), challenge.ChallengeID)
}

// TestAccount_CreateMagicURLSession_NoSecretInResponse 断言响应中的
// challengeID 不可用作登录 secret（仅邮件链接中的 secret 有效）。
func TestAccount_CreateMagicURLSession_NoSecretInResponse(t *testing.T) {
	f := setupMagicURL(t, true)

	challenge, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)
	linkSecret := f.linkSecret(t)
	require.NotEmpty(t, challenge.ChallengeID)
	require.NotEqual(t, linkSecret, challenge.ChallengeID)

	// 用响应里的 challengeID 当 secret 无法登录（该次尝试也会原子消费掉
	// 这条 token 记录，因此成功路径需重新发起一次 magic url）。
	_, _, _, _, err = f.account.UpdateMagicURLSession(f.ctx, UpdateMagicURLSessionCommand{
		ProjectID: f.projectID,
		UserID:    f.userID,
		Secret:    challenge.ChallengeID,
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())

	// 重新发起：邮件链接里的 secret 可正常登录（先越过 send cooldown）。
	f.mr.FastForward(time.Minute)
	_, err = f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)
	user, tokens, cookie, mfa, err := f.account.UpdateMagicURLSession(f.ctx, UpdateMagicURLSessionCommand{
		ProjectID: f.projectID,
		UserID:    f.userID,
		Secret:    f.linkSecret(t),
	})
	require.NoError(t, err)
	require.Equal(t, f.userID, user.ID)
	require.NotEmpty(t, tokens.AccessToken)
	require.NotEmpty(t, cookie)
	require.Nil(t, mfa)
}

func TestAccount_CreateMagicURLSession_AntiEnumeration(t *testing.T) {
	f := setupMagicURL(t, true)

	// 不存在的用户：空响应、不发信、无错误。
	challenge, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "nobody@torchwood.local",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)
	require.Empty(t, challenge.ChallengeID)
	require.Len(t, f.mailer.Bodies, 0)
}

func TestAccount_CreateMagicURLSession_PlaceholderEmailRejected(t *testing.T) {
	f := setupMagicURL(t, true)

	// 匿名/占位邮箱不允许 magic url 登录。
	challenge, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "anon_abcd@torchwood.local",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)
	require.Empty(t, challenge.ChallengeID)
	require.Len(t, f.mailer.Bodies, 0)
}

func TestAccount_CreateMagicURLSession_RedirectAllowlist(t *testing.T) {
	f := setupMagicURL(t, true)

	// 未在白名单的 URL 拒绝（默认白名单只含 public base URL）。
	_, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://evil.example.com/phish",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestAccount_CreateMagicURLSession_NoMailer(t *testing.T) {
	f := setupMagicURL(t, false)
	// 无 mailer 装配 → Unimplemented。
	f.account.mailer = nil
	_, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://app.example.com/signin",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestAccount_UpdateMagicURLSession_SignIn(t *testing.T) {
	f := setupMagicURL(t, true)

	_, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)

	user, tokens, cookie, mfa, err := f.account.UpdateMagicURLSession(f.ctx, UpdateMagicURLSessionCommand{
		ProjectID: f.projectID,
		UserID:    f.userID,
		Secret:    f.linkSecret(t),
	})
	require.NoError(t, err)
	require.Equal(t, f.userID, user.ID)
	require.NotEmpty(t, tokens.AccessToken)
	require.NotEmpty(t, cookie)
	require.Nil(t, mfa)

	// 一次性：二次使用拒绝。
	_, _, _, _, err = f.account.UpdateMagicURLSession(f.ctx, UpdateMagicURLSessionCommand{
		ProjectID: f.projectID,
		UserID:    f.userID,
		Secret:    f.linkSecret(t),
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAccount_UpdateMagicURLSession_WrongSecret(t *testing.T) {
	f := setupMagicURL(t, true)

	_, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)

	_, _, _, _, err = f.account.UpdateMagicURLSession(f.ctx, UpdateMagicURLSessionCommand{
		ProjectID: f.projectID,
		UserID:    f.userID,
		Secret:    strings.Repeat("0", 64),
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAccount_UpdateMagicURLSession_Expired(t *testing.T) {
	f := setupMagicURL(t, true)

	challenge, err := f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)
	require.NotEmpty(t, challenge.ChallengeID)

	f.mr.FastForward(61 * time.Minute) // > 1h TTL

	_, _, _, _, err = f.account.UpdateMagicURLSession(f.ctx, UpdateMagicURLSessionCommand{
		ProjectID: f.projectID,
		UserID:    f.userID,
		Secret:    f.linkSecret(t),
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAccount_UpdateMagicURLSession_MFARequired(t *testing.T) {
	f := setupMagicURL(t, true)

	// 创建并激活 TOTP 因子。
	factor, plainSecret, _, err := f.account.CreateTOTPFactor(f.userContext(), f.projectID, f.userID, "")
	require.NoError(t, err)
	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	_, err = f.account.VerifyTOTPFactor(f.userContext(), f.projectID, f.userID, factor.ID, code)
	require.NoError(t, err)

	_, err = f.account.CreateMagicURLSession(f.ctx, CreateMagicURLSessionCommand{
		ProjectID: f.projectID,
		Email:     "magic-user@example.com",
		URL:       "https://app.example.com/signin",
	})
	require.NoError(t, err)

	user, tokens, cookie, mfa, err := f.account.UpdateMagicURLSession(f.ctx, UpdateMagicURLSessionCommand{
		ProjectID: f.projectID,
		UserID:    f.userID,
		Secret:    f.linkSecret(t),
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	require.Nil(t, tokens)
	require.Empty(t, cookie)
	require.NotNil(t, mfa)
	require.NotEmpty(t, mfa.Token)
	require.Len(t, mfa.Factors, 1)
}
