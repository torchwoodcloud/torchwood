package client

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func setupAccountSecurity(t *testing.T, withRedis bool) (context.Context, *Account, string, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	projectRepo := bunrepo.NewProjectRepository(db)

	var account *Account
	if withRedis {
		mr, err := miniredis.Run()
		require.NoError(t, err)
		t.Cleanup(mr.Close)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		account = NewTestAccountWithRedis(securityTestConfig(), projectRepo, db, rdb)
	} else {
		account = NewTestAccount(securityTestConfig(), projectRepo, db)
	}
	return ctx, account, projectID, ""
}

func securityTestConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "account-security-test-secret"},
		},
	}
}

// TestAccount_UpdateEmailRequiresOldPasswordAndStages（R05-P1-2 A 档）：改邮箱
// 必须带 url + 旧密码（非匿名）；成功后走 staging——email 保持旧值、会话不撤销
// （撤会话推迟到 ConfirmEmailChange），旧邮箱仍可登录、新邮箱不可。
func TestAccount_UpdateEmailRequiresOldPasswordAndStages(t *testing.T) {
	ctx, account, projectID, _, _, _ := setupG3Account(t)

	user, userID := signUpG3User(t, ctx, account, projectID, "email-change@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + userID},
	})

	// 改邮箱不带 url → 拒绝。
	_, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("new-email@torchwood.local"),
		OldPassword: "User@123",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 无旧密码 → 拒绝。
	_, err = account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email: strPtr("new-email@torchwood.local"),
		URL:   "http://localhost/confirm-email",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 旧密码错误 → 拒绝。
	_, err = account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("new-email@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "wrong",
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// 旧密码正确 → staging：email 保持旧值、会话不撤销。
	updated, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:       strPtr("new-email@torchwood.local"),
		URL:         "http://localhost/confirm-email",
		OldPassword: "User@123",
	})
	require.NoError(t, err)
	require.Equal(t, "email-change@torchwood.local", updated.Email, "staging 阶段 email 必须保持旧值")

	sessions, err := account.ListSessions(authCtx)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "staging 阶段不得撤销会话（旧邮箱仍可登录）")

	// 旧邮箱仍可登录；新邮箱不可。
	_, tokens, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "email-change@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)
	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "new-email@torchwood.local",
		Password:  "User@123",
	})
	require.Error(t, err)
}

// TestAccount_AnonymousUpgradeSetsPasswordWithoutOldPassword：匿名用户
// （password_hash 为空）升级时跳过 old_password；密码立即生效并撤销会话，
// 邮箱变更走 staging（确认前保持占位邮箱）。
func TestAccount_AnonymousUpgradeSetsPasswordWithoutOldPassword(t *testing.T) {
	ctx, account, projectID, _, _, _ := setupG3Account(t)

	anon, _, _, _, err := account.CreateAnonymousSession(ctx, CreateAnonymousSessionCommand{ProjectID: projectID})
	require.NoError(t, err)

	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    anon.ID,
		Email:     anon.Email,
		Roles:     []string{"users", "user:" + anon.ID},
	})

	updated, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Email:    strPtr("upgraded@example.com"),
		URL:      "http://localhost/confirm-email",
		Password: "NewPass@123",
	})
	require.NoError(t, err)
	require.Equal(t, anon.Email, updated.Email, "邮箱变更走 staging，确认前保持占位邮箱")

	// 密码已生效且会话已撤销（password 变更路径撤会话）：占位邮箱 + 新密码可登录。
	_, tokens, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     anon.Email,
		Password:  "NewPass@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.AccessToken)

	// 新邮箱 staging 未生效，不可登录。
	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "upgraded@example.com",
		Password:  "NewPass@123",
	})
	require.Error(t, err)
}

func TestAccount_UpdatePrefsLimits(t *testing.T) {
	ctx, account, projectID, _ := setupAccountSecurity(t, false)

	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "prefs-limit@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})

	// 超过 64KB → 拒绝。
	huge := map[string]any{"blob": strings.Repeat("x", 64*1024)}
	_, err = account.UpdatePrefs(authCtx, huge)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())

	// 嵌套深度 > 20 → 拒绝。
	deep := map[string]any{}
	cur := deep
	for i := 0; i < 21; i++ {
		next := map[string]any{}
		cur["n"] = next
		cur = next
	}
	_, err = account.UpdatePrefs(authCtx, deep)
	require.Error(t, err)
	st, _ = status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())

	// 正常 prefs 可用。
	got, err := account.UpdatePrefs(authCtx, map[string]any{"theme": "dark"})
	require.NoError(t, err)
	require.Equal(t, "dark", got["theme"])
}

func TestAccount_SignUpRateLimit(t *testing.T) {
	ctx, account, projectID, _ := setupAccountSecurity(t, true)

	clientCtx := contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: "203.0.113.7"})
	for i := 0; i < defaultSignUpIPLimit; i++ {
		_, _, _, _, err := account.SignUp(clientCtx, SignUpCommand{
			ProjectID: projectID,
			Email:     fmt.Sprintf("signup-%d@torchwood.local", i),
			Password:  "User@123",
		})
		require.NoError(t, err)
	}
	_, _, _, _, err := account.SignUp(clientCtx, SignUpCommand{
		ProjectID: projectID,
		Email:     "signup-over@torchwood.local",
		Password:  "User@123",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.ResourceExhausted, st.Code())

	// 其他 IP 不受影响。
	otherCtx := contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: "203.0.113.8"})
	_, _, _, _, err = account.SignUp(otherCtx, SignUpCommand{
		ProjectID: projectID,
		Email:     "signup-other@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
}

func TestAccount_EmailValidation(t *testing.T) {
	ctx, account, projectID, _ := setupAccountSecurity(t, false)

	_, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "not-an-email",
		Password:  "User@123",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())

	// 超过 254 字符 → 拒绝。
	long := strings.Repeat("a", 245) + "@example.com"
	_, _, _, _, err = account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     long,
		Password:  "User@123",
	})
	require.Error(t, err)
	st, _ = status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}
