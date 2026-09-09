package client

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// setupThrottleAccount 装配带 Redis 登录频控的 Account（T-01 验收场景）。
func setupThrottleAccount(t *testing.T) (context.Context, *Account, string, *miniredis.Miniredis) {
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
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	account := NewTestAccountWithRedis(securityTestConfig(), projectRepo, db, rdb)
	return ctx, account, projectID, mr
}

func signInErr(t *testing.T, ctx context.Context, account *Account, projectID, email, password, ip string) error {
	t.Helper()
	_, _, _, _, err := account.SignIn(contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: ip}), SignInCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  password,
	})
	return err
}

// TestAccount_SignInThrottleLimitAndReset（T-01 验收）：同账号连续失败在
// 第 6 次调用触发 429；正确密码在未触限时可正常登录且登录成功计数归零
// （若未归零，重置后第 6 次失败之前就会提前触发）。
func TestAccount_SignInThrottleLimitAndReset(t *testing.T) {
	t.Parallel()
	ctx, account, projectID, _ := setupThrottleAccount(t)

	const email, ip = "throttle@torchwood.local", "203.0.113.10"
	_, _, _, _, err := account.SignUp(contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: ip}), SignUpCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123",
	})
	require.NoError(t, err)

	// 两次失败 → 正确密码登录成功（重置计数）。
	require.Equal(t, codes.Unauthenticated, status.Code(signInErr(t, ctx, account, projectID, email, "wrong", ip)))
	require.Equal(t, codes.Unauthenticated, status.Code(signInErr(t, ctx, account, projectID, email, "wrong", ip)))
	_, tokens, _, _, err := account.SignIn(contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: ip}), SignInCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123",
	})
	require.NoError(t, err, "未触限时正确密码必须可登录")
	require.NotEmpty(t, tokens.AccessToken)

	// 重置后第 6 次失败 → 429（若第 1 步计数未重置，此处会提前触发）。
	for i := 0; i < 5; i++ {
		err = signInErr(t, ctx, account, projectID, email, "wrong", ip)
		require.Equal(t, codes.Unauthenticated, status.Code(err), "第 %d 次失败未触限时仍为 401", i+1)
	}
	err = signInErr(t, ctx, account, projectID, email, "User@123", ip)
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "第 6 次调用必须 429")
	st, _ := status.FromError(err)
	require.Equal(t, "too many failed sign-in attempts, try again later", st.Message())
}

// TestAccount_SignInThrottleNoEnumeration（T-01 验收：429 不泄露账号存在性）：
// 探测不存在账号与暴力真实账号，在相同强度下产生相同 code+message 的 429
// （IP 维度独立于账号存在性计数）。
func TestAccount_SignInThrottleNoEnumeration(t *testing.T) {
	t.Parallel()
	ctx, account, projectID, _ := setupThrottleAccount(t)

	// 真实账号 + 错误密码（邮箱+IP 双维计数）。
	const realEmail, ipA = "real@torchwood.local", "203.0.113.11"
	_, _, _, _, err := account.SignUp(contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: ipA}), SignUpCommand{
		ProjectID: projectID,
		Email:     realEmail,
		Password:  "User@123",
	})
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		err = signInErr(t, ctx, account, projectID, realEmail, "wrong", ipA)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.Equal(t, "invalid credentials", status.Convert(err).Message())
	}
	exhaustedReal := signInErr(t, ctx, account, projectID, realEmail, "wrong", ipA)
	require.Equal(t, codes.ResourceExhausted, status.Code(exhaustedReal))

	// 不存在账号 + 任意密码（仅 IP 维度计数）：相同强度下同样 429 且文案一致。
	const ghostEmail, ipB = "ghost@torchwood.local", "203.0.113.12"
	for i := 0; i < 5; i++ {
		err = signInErr(t, ctx, account, projectID, ghostEmail, "whatever", ipB)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.Equal(t, "invalid credentials", status.Convert(err).Message(), "401 语义必须与存在账号一致")
	}
	exhaustedGhost := signInErr(t, ctx, account, projectID, ghostEmail, "whatever", ipB)
	require.Equal(t, codes.ResourceExhausted, status.Code(exhaustedGhost))
	require.Equal(t, status.Convert(exhaustedReal).Message(), status.Convert(exhaustedGhost).Message())
}

// TestAccount_SignInThrottleAuditRow：触发限速时写审计行（Status="throttled"、
// 带项目与 IP；T-01）。
func TestAccount_SignInThrottleAuditRow(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)
	projectRepo := bunrepo.NewProjectRepository(db)
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	account := NewTestAccountWithRedis(securityTestConfig(), projectRepo, db, rdb)
	auditRepo := bunrepo.NewAuditRepository(db)
	account.auditRepo = auditRepo

	const email, ip = "audit@torchwood.local", "203.0.113.13"
	_, _, _, _, err = account.SignUp(contexts.WithClientInfo(ctx, contexts.ClientInfo{IP: ip}), SignUpCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123",
	})
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_ = signInErr(t, ctx, account, projectID, email, "wrong", ip)
	}
	require.Equal(t, codes.ResourceExhausted, status.Code(signInErr(t, ctx, account, projectID, email, "wrong", ip)))

	var rows []model.AuditLog
	err = db.NewSelect().Model(&rows).
		Where("project_id = ? AND action = ? AND status = 'throttled'", projectID, auditActionSignIn).
		Scan(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "触发限速必须恰好写一条 throttled 审计行")
	require.Equal(t, ip, rows[0].IP)
	require.Equal(t, auditActionSignIn, rows[0].Action)
}
