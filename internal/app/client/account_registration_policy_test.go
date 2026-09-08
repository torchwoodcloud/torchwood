package client

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	infraauth "github.com/torchwooddev/torchwood/internal/infra/auth"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// setupRegistrationAccount 装配带真实邀请码仓储的 Account（T-03 验收场景），
// 返回 db 便于直接操纵项目注册策略与邀请码表。
func setupRegistrationAccount(t *testing.T) (context.Context, *Account, string, *clients.Database, projects.InviteCodeRepository, projects.Repository) {
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
	inviteRepo := bunrepo.NewInviteCodeRepository(db)
	account := NewTestAccount(securityTestConfig(), projectRepo, db)
	account.inviteRepo = inviteRepo
	return ctx, account, projectID, db, inviteRepo, projectRepo
}

func setRegistrationPolicy(t *testing.T, projectRepo projects.Repository, projectID, policy string) {
	t.Helper()
	p, err := projectRepo.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	require.NotNil(t, p)
	p.RegistrationPolicy = policy
	require.NoError(t, projectRepo.UpdateProject(context.Background(), p))
}

// TestAccount_SignUp_ClosedProject（T-03 验收）：closed 项目 SignUp → 403
// ACCOUNT.REGISTRATION_CLOSED；invite_code 字段被忽略（closed 不消费任何码）。
func TestAccount_SignUp_ClosedProject(t *testing.T) {
	t.Parallel()
	ctx, account, projectID, _, _, projectRepo := setupRegistrationAccount(t)
	setRegistrationPolicy(t, projectRepo, projectID, projects.RegistrationClosed)

	err := func() error {
		_, _, _, _, err := account.SignUp(ctx, SignUpCommand{
			ProjectID:  projectID,
			Email:      "closed-proj@torchwood.local",
			Password:   "User@123",
			InviteCode: "twi_whatever",
		})
		return err
	}()
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.True(t, strings.HasPrefix(status.Convert(err).Message(), "ACCOUNT.REGISTRATION_CLOSED"))
}

// TestAccount_SignUp_InviteOnly（T-03 验收）：无码/错码 403；正确码注册成功
// 且一次性码作废；过期码 403。
func TestAccount_SignUp_InviteOnly(t *testing.T) {
	t.Parallel()
	ctx, account, projectID, _, inviteRepo, projectRepo := setupRegistrationAccount(t)
	setRegistrationPolicy(t, projectRepo, projectID, projects.RegistrationInviteOnly)

	signUpErr := func(code string) error {
		_, _, _, _, err := account.SignUp(ctx, SignUpCommand{
			ProjectID:  projectID,
			Email:      "invite@torchwood.local",
			Password:   "User@123",
			InviteCode: code,
		})
		return err
	}

	// 无码 / 错码 → 403 ACCOUNT.INVITE_CODE_INVALID。
	require.Equal(t, codes.PermissionDenied, status.Code(signUpErr("")))
	require.Equal(t, codes.PermissionDenied, status.Code(signUpErr("twi_wrong")))
	err := signUpErr("twi_wrong")
	require.True(t, strings.HasPrefix(status.Convert(err).Message(), "ACCOUNT.INVITE_CODE_INVALID"))

	// 正确码 → 成功；一次性码复用 → 403。
	codeID := "ic-1"
	require.NoError(t, inviteRepo.CreateInviteCode(ctx, &projects.InviteCode{
		ID:        codeID,
		ProjectID: projectID,
		Code:      "twi_oneshot",
		MaxUses:   1,
	}))
	_, _, _, _, err = account.SignUp(ctx, SignUpCommand{
		ProjectID:  projectID,
		Email:      "invite-ok@torchwood.local",
		Password:   "User@123",
		InviteCode: "twi_oneshot",
	})
	require.NoError(t, err)
	err = signUpErr("twi_oneshot")
	require.Equal(t, codes.PermissionDenied, status.Code(err), "一次性码第二次使用必须 403")

	// 过期码 → 403。
	past := time.Now().Add(-time.Hour)
	require.NoError(t, inviteRepo.CreateInviteCode(ctx, &projects.InviteCode{
		ID:        "ic-expired",
		ProjectID: projectID,
		Code:      "twi_expired",
		MaxUses:   1,
		ExpireAt:  &past,
	}))
	require.Equal(t, codes.PermissionDenied, status.Code(signUpErr("twi_expired")))
}

// TestAccount_SignUp_InviteCodeConcurrent（T-03 验收：并发同码只有一个成功）。
func TestAccount_SignUp_InviteCodeConcurrent(t *testing.T) {
	t.Parallel()
	ctx, account, projectID, _, inviteRepo, projectRepo := setupRegistrationAccount(t)
	setRegistrationPolicy(t, projectRepo, projectID, projects.RegistrationInviteOnly)

	require.NoError(t, inviteRepo.CreateInviteCode(ctx, &projects.InviteCode{
		ID:        "ic-race",
		ProjectID: projectID,
		Code:      "twi_race",
		MaxUses:   1,
	}))

	const goroutines = 2
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, _, errs[i] = account.SignUp(ctx, SignUpCommand{
				ProjectID:  projectID,
				Email:      "race-" + string(rune('a'+i)) + "@torchwood.local",
				Password:   "User@123",
				InviteCode: "twi_race",
			})
		}(i)
	}
	wg.Wait()

	success := 0
	for _, err := range errs {
		if err == nil {
			success++
		}
	}
	require.Equal(t, 1, success, "并发同码必须恰好一个成功（got errs=%v）", errs)
}

// TestAccount_DeleteAccount（T-03 验收）：注销后 token 立即 401、SignIn 仍
// 统一 invalid credentials、同邮箱可立即重新注册、会话清空。
func TestAccount_DeleteAccount(t *testing.T) {
	t.Parallel()
	ctx, account, projectID, testDB, _, _ := setupRegistrationAccount(t)

	const email = "doomed@torchwood.local"
	user, tokens, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123",
	})
	require.NoError(t, err)

	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		SessionID: "sess-doomed",
		Email:     email,
		Roles:     []string{"users", "user:" + user.ID},
	})

	// token 校验器（与生产同构）：端用户 JWT 经 ensureUserCanAuthenticate
	// 实时读库判定 CanAuthenticate。
	usersRepo := bunrepo.NewUserRepository(testDB)
	validator := infraauth.NewValidator(securityTestConfig(), nil, nil, nil, nil, nil,
		bunrepo.NewSessionRepository(testDB), usersRepo, nil)

	// 注销前：token 校验通过、会话存在。
	_, err = validator.ValidateCredential(ctx, tokens.AccessToken, shared.CredentialTypeToken)
	require.NoError(t, err)
	sessions, err := account.ListSessions(authCtx)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)

	require.NoError(t, account.DeleteAccount(authCtx))

	// 凭据立即失效：users 行 status=deleted → CanAuthenticate=false → 401。
	_, err = validator.ValidateCredential(ctx, tokens.AccessToken, shared.CredentialTypeToken)
	require.Equal(t, codes.Unauthenticated, status.Code(err), "deleted 账号的 token 必须立即失效")

	// SignIn 拒绝且保持统一 invalid credentials（email 已清洗，查无此人）。
	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123",
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	require.Equal(t, "invalid credentials", status.Convert(err).Message())

	// 同邮箱可立即重新注册（不泄露"曾存在"）。
	_, _, _, _, err = account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123",
	})
	require.NoError(t, err, "同邮箱必须可立即重新注册")

	// 被删账号的会话已清空。
	oldSessions, err := account.ListSessions(authCtx)
	require.NoError(t, err)
	require.Empty(t, oldSessions)
}
