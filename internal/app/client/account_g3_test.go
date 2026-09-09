package client

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	inframessaging "github.com/torchwoodcloud/torchwood/internal/infra/messaging"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// strPtr 供 D-1 presence 语义下构造 *string 字段（nil=不修改）的测试用。
func strPtr(s string) *string { return &s }

// failableSessionService 包装真实 SessionService，DeleteSessionsByUser 可按
// 标志注入故障（R05-P1-3 故障注入测试用）。
type failableSessionService struct {
	real domainauth.SessionService
	fail bool
}

func (f *failableSessionService) CreateSessionAndTokens(ctx context.Context, projectID, userID, email, provider string) (*domainauth.TokenBundle, string, error) {
	return f.real.CreateSessionAndTokens(ctx, projectID, userID, email, provider)
}

func (f *failableSessionService) CreateShortLivedSessionAndTokens(ctx context.Context, projectID, userID, email, provider string) (*domainauth.TokenBundle, string, error) {
	return f.real.CreateShortLivedSessionAndTokens(ctx, projectID, userID, email, provider)
}

func (f *failableSessionService) IssueTokens(ctx context.Context, projectID, userID, email, sessionID string) (*domainauth.TokenBundle, string, error) {
	return f.real.IssueTokens(ctx, projectID, userID, email, sessionID)
}

func (f *failableSessionService) IssueTokensWithRefreshID(ctx context.Context, projectID, userID, email, sessionID, refreshTokenID string) (*domainauth.TokenBundle, string, error) {
	return f.real.IssueTokensWithRefreshID(ctx, projectID, userID, email, sessionID, refreshTokenID)
}

func (f *failableSessionService) EnsureActiveSession(ctx context.Context, projectID, sessionID, userID string) error {
	return f.real.EnsureActiveSession(ctx, projectID, sessionID, userID)
}

func (f *failableSessionService) DeleteSessionsByUser(ctx context.Context, projectID, userID string) error {
	if f.fail {
		return status.Error(codes.Internal, "injected session revocation failure")
	}
	return f.real.DeleteSessionsByUser(ctx, projectID, userID)
}

// setupG3Account 组装带 miniredis + 可选 mailer + 可注入故障的 session
// service 的 Account（真实 Postgres docDB，集成测试，-short 跳过）。
func setupG3Account(t *testing.T) (context.Context, *Account, string, *failableSessionService, *miniredis.Miniredis, *CaptureMailer) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	// M5 C8：匿名会话限流 fail-closed，测试上下文统一注入 ClientInfo。
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.10"})
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "g3-account-test-secret"},
		},
	}
	projectRepo := bunrepo.NewProjectRepository(db)
	usersRepo := bunrepo.NewUserRepository(db)
	sessionRepo := bunrepo.NewSessionRepository(db)
	identities := bunrepo.NewIdentityRepository(db)
	roles := NewUserRoles(usersRepo, bunrepo.NewMembershipRepository(db))
	rotation := auth.NewRedisRefreshRotationStore(rdb)
	realSessions := auth.NewSessionService(cfg, sessionRepo, roles, rotation)
	sessions := &failableSessionService{real: realSessions}
	mailer := &CaptureMailer{}
	account := NewAccount(
		cfg,
		projectRepo,
		nil, // inviteRepo
		nil,
		sessions,
		auth.NewRedisOTPChallengeStore(rdb, cfg),
		auth.NewRedisOAuthStateStore(rdb),
		auth.NewRedisAccountTokenStore(rdb),
		auth.NewRedisLoginThrottle(rdb),
		rotation,
		nil,
		mailer,
		inframessaging.NewSMSService(cfg),
		auth.NewRedisRateLimiter(rdb),
		roles,
		auth.NewTOTPService(cfg, rdb),
		auth.NewRedisMFAChallengeStore(rdb),
		auth.NewRedisOneTimeTokenStore(rdb),
		nil,
		usersRepo,
		identities,
		sessionRepo,
		auth.NewOAuthAuthenticatorFactory(),
		auth.NewWeChatMiniProgramExchanger(),
		auth.NewOTPGenerator(),
		nil,
	)
	return ctx, account, projectID, sessions, mr, mailer
}

func signUpG3User(t *testing.T, ctx context.Context, account *Account, projectID, email string) (*User, string) {
	t.Helper()
	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123",
		Name:      "G3 User",
	})
	require.NoError(t, err)
	return user, user.ID
}

// TestAccount_EmailChangeNotifiesOldEmail（R05-P1-2，A 档 staging）：改邮箱
// 走 pending：email 保持旧值，新邮箱收到验证邮件、旧邮箱收到安全通知；
// 验证通过（ConfirmEmailChange）后才切换。
func TestAccount_EmailChangeNotifiesOldEmail(t *testing.T) {
	ctx, account, projectID, _, _, mailer := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "old-mail@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	updated, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("new-mail@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "User@123",
	})
	require.NoError(t, err)
	require.Equal(t, "old-mail@torchwood.local", updated.Email, "验证通过前 email 必须保持旧值")
	require.False(t, updated.EmailVerified)

	// 新邮箱收到验证邮件（含确认链接）。
	require.Equal(t, []string{"new-mail@torchwood.local", "old-mail@torchwood.local"}, mailer.To)
	require.Contains(t, mailer.Subjects[0], "Confirm your Torchwood email change")
	require.Contains(t, mailer.Bodies[0], "http://localhost/confirm-email")
	// 旧邮箱收到安全通知（B 档成果保留）。
	require.Contains(t, mailer.Subjects[1], "email address is being changed")
	require.Contains(t, mailer.Bodies[1], "did not make this change")
}

// TestAccount_UpdateAccount_SessionRevocationFailureLeavesCredentials（R05-P1-3）：
// 撤会话失败时资料变更不得提交——旧密码仍可登录、邮箱不变。
func TestAccount_UpdateAccount_SessionRevocationFailureLeavesCredentials(t *testing.T) {
	ctx, account, projectID, sessions, _, _ := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "revoke-fail@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	sessions.fail = true
	_, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Password:    "NewPass@123",
		OldPassword: "User@123",
	})
	require.Error(t, err)

	// 旧密码仍可登录（password_hash 未变），邮箱未变。
	sessions.fail = false
	_, tokens, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "revoke-fail@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)

	// 邮箱变更走 staging：pending 阶段不撤会话，UpdateAccount 不再因撤会话
	// 失败而报错；撤会话语义由 ConfirmEmailChange 的故障注入测试覆盖。
	sessions.fail = true
	_, err = account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("staged@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "User@123",
	})
	require.NoError(t, err)
	// 邮箱仍为旧值（staging 未切换），且旧密码仍可登录。
	sessions.fail = false
	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "revoke-fail@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
}

// TestAccount_UpdateRecovery_SessionRevocationFailureLeavesOldPassword（R05-P1-3）：
// 找回密码路径撤会话失败时旧密码保持有效。
func TestAccount_UpdateRecovery_SessionRevocationFailureLeavesOldPassword(t *testing.T) {
	ctx, account, projectID, sessions, _, _ := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "recovery-revoke-fail@torchwood.local")

	secret, _, err := account.tokens.CreateRecoveryToken(ctx, projectID, userID, user.Email)
	require.NoError(t, err)

	sessions.fail = true
	err = account.UpdateRecovery(ctx, UpdateRecoveryCommand{
		ProjectID: projectID,
		UserID:    userID,
		Secret:    secret,
		Password:  "NewPass@456",
	})
	require.Error(t, err)

	// 旧密码仍可登录。
	sessions.fail = false
	_, tokens, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "recovery-revoke-fail@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)
}

// TestAccount_UnregisteredEmailFailuresIPOnlyThrottle（R05-P1-5 + T-01 裁决）：
// 未注册邮箱的失败只计 IP 维度（邮箱键永不落笔）——前 5 次 401 统一语义，
// 第 6 次经 IP 维度触发 429；其他 IP 上的已注册用户登录不受影响。
func TestAccount_UnregisteredEmailFailuresIPOnlyThrottle(t *testing.T) {
	ctx, account, projectID, _, _, _ := setupG3Account(t)

	signUpG3User(t, ctx, account, projectID, "registered-user@torchwood.local")

	attackerCtx := contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: "198.51.100.77"})
	for i := 0; i < 5; i++ {
		_, _, _, _, err := account.SignIn(attackerCtx, SignInCommand{
			ProjectID: projectID,
			Email:     "no-such-user@torchwood.local",
			Password:  "WrongPass@1",
		})
		require.Error(t, err)
		st, _ := status.FromError(err)
		require.Equal(t, codes.Unauthenticated, st.Code(), "未触限前必须是统一 Unauthenticated")
	}
	// 第 6 次：IP 维度触发 429（未注册邮箱也计数，429 不构成存在性 oracle）。
	_, _, _, _, err := account.SignIn(attackerCtx, SignInCommand{
		ProjectID: projectID,
		Email:     "no-such-user@torchwood.local",
		Password:  "WrongPass@1",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.ResourceExhausted, st.Code())

	// 其他 IP 上的已注册用户不受影响（邮箱键未被探测行为污染）。
	userCtx := contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: "198.51.100.78"})
	_, tokens, _, _, err := account.SignIn(userCtx, SignInCommand{
		ProjectID: projectID,
		Email:     "registered-user@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)
}

// ---- R05-P1-2 A 档 staging：ConfirmEmailChange ----

// confirmEmailChangeSecret 从验证邮件 body 中提取确认链接里的 secret
// （模拟真实用户点击邮件链接）。
func confirmEmailChangeSecret(t *testing.T, body string) string {
	t.Helper()
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "http") {
			u, err := url.Parse(field)
			require.NoError(t, err)
			secret := u.Query().Get("secret")
			require.NotEmpty(t, secret, "确认链接必须携带 secret")
			return secret
		}
	}
	t.Fatal("验证邮件中找不到确认链接")
	return ""
}

// TestAccount_EmailChangeStaging_OldEmailWorksUntilConfirm（R05-P1-2 验收）：
// SignUp → UpdateAccount 改邮箱 → 旧邮箱仍可登录、新邮箱登录失败 →
// ConfirmEmailChange → 新邮箱生效、旧邮箱失效。
func TestAccount_EmailChangeStaging_OldEmailWorksUntilConfirm(t *testing.T) {
	ctx, account, projectID, _, _, mailer := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "stage-old@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	_, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("stage-new@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "User@123",
	})
	require.NoError(t, err)

	// 未验证前：旧邮箱可登录，新邮箱不可。
	_, tokens, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "stage-old@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)
	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "stage-new@torchwood.local",
		Password:  "User@123",
	})
	require.Error(t, err)

	secret := confirmEmailChangeSecret(t, mailer.Bodies[0])
	confirmed, err := account.ConfirmEmailChange(authCtx, ConfirmEmailChangeCommand{
		ProjectID: projectID,
		UserID:    userID,
		Secret:    secret,
	})
	require.NoError(t, err)
	require.Equal(t, "stage-new@torchwood.local", confirmed.Email)
	require.True(t, confirmed.EmailVerified)

	// 切换后：新邮箱可登录，旧邮箱失效。
	_, tokens, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "stage-new@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)
	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "stage-old@torchwood.local",
		Password:  "User@123",
	})
	require.Error(t, err)
}

// TestAccount_ConfirmEmailChange_TokenOneTime：token 一次性——二次使用返回
// Unauthenticated。
func TestAccount_ConfirmEmailChange_TokenOneTime(t *testing.T) {
	ctx, account, projectID, _, _, mailer := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "onetime@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	_, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("onetime-new@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "User@123",
	})
	require.NoError(t, err)

	secret := confirmEmailChangeSecret(t, mailer.Bodies[0])
	_, err = account.ConfirmEmailChange(authCtx, ConfirmEmailChangeCommand{
		ProjectID: projectID,
		UserID:    userID,
		Secret:    secret,
	})
	require.NoError(t, err)

	_, err = account.ConfirmEmailChange(authCtx, ConfirmEmailChangeCommand{
		ProjectID: projectID,
		UserID:    userID,
		Secret:    secret,
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err), "token 二次使用必须拒绝")
}

// TestAccount_ConfirmEmailChange_NewEmailTaken：新邮箱在 token 有效期内被他人
// 注册（pending_email 不占用 email 唯一约束，SignUp 查重查不到）→ 确认时
// AlreadyExists。
func TestAccount_ConfirmEmailChange_NewEmailTaken(t *testing.T) {
	ctx, account, projectID, _, _, mailer := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "changer@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	_, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("taken@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "User@123",
	})
	require.NoError(t, err)

	secret := confirmEmailChangeSecret(t, mailer.Bodies[0])

	// 确认前新邮箱被他人注册（竞态窗口：pending 不占 email 唯一约束）。
	signUpG3User(t, ctx, account, projectID, "taken@torchwood.local")

	_, err = account.ConfirmEmailChange(authCtx, ConfirmEmailChangeCommand{
		ProjectID: projectID,
		UserID:    userID,
		Secret:    secret,
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

// TestAccount_ConfirmEmailChange_SessionRevocationFailureLeavesOldEmail（G3-3
// 语义在确认路径）：撤会话失败时邮箱保持旧值、不提交。
func TestAccount_ConfirmEmailChange_SessionRevocationFailureLeavesOldEmail(t *testing.T) {
	ctx, account, projectID, sessions, _, mailer := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "revoke-confirm@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	_, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("revoke-confirm-new@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "User@123",
	})
	require.NoError(t, err)

	secret := confirmEmailChangeSecret(t, mailer.Bodies[0])
	sessions.fail = true
	_, err = account.ConfirmEmailChange(authCtx, ConfirmEmailChangeCommand{
		ProjectID: projectID,
		UserID:    userID,
		Secret:    secret,
	})
	require.Error(t, err)

	// 邮箱未切换，旧邮箱仍可登录。
	sessions.fail = false
	_, tokens, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "revoke-confirm@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)
}

// TestAccount_ConfirmEmailChange_PublicAccess：ConfirmEmailChange 为免登录
// （ACCESS_PUBLIC，点邮件链接即完成）——无 principal 上下文也能确认成功，
// 安全模型与 recovery 一致（随机 secret + TTL + GETDEL 一次性）。
func TestAccount_ConfirmEmailChange_PublicAccess(t *testing.T) {
	ctx, account, projectID, _, _, mailer := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "own-check@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	_, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("own-check-new@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "User@123",
	})
	require.NoError(t, err)

	// 无 principal 的裸上下文（模拟用户在新浏览器点开邮件链接）：确认成功。
	secret := confirmEmailChangeSecret(t, mailer.Bodies[0])
	confirmed, err := account.ConfirmEmailChange(ctx, ConfirmEmailChangeCommand{
		ProjectID: projectID,
		UserID:    userID,
		Secret:    secret,
	})
	require.NoError(t, err)
	require.Equal(t, "own-check-new@torchwood.local", confirmed.Email)

	// 新邮箱可登录、旧邮箱失效。
	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "own-check-new@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "own-check@torchwood.local",
		Password:  "User@123",
	})
	require.Error(t, err)
}

// TestAccount_UpdateAccount_EmailChangeRequiresURL：改邮箱时 url 必填。
func TestAccount_UpdateAccount_EmailChangeRequiresURL(t *testing.T) {
	ctx, account, projectID, _, _, _ := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "need-url@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	_, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("need-url-new@torchwood.local"),
		OldPassword: "User@123",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "改邮箱不带 url 必须拒绝")
}
