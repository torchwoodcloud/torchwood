package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func mfaTestConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "mfa-app-test-secret"}, // #nosec G101 -- 测试固定值
		},
	}
}

// setupMFATestAccount 注册一个用户并返回带 principal 的 ctx、account、miniredis。
func setupMFATestAccount(t *testing.T) (context.Context, *Account, string, string, *miniredis.Miniredis) {
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

	projectRepo := bunrepo.NewProjectRepository(db)

	account := NewTestAccountWithRedis(mfaTestConfig(), projectRepo, db, rdb)

	user, _, _, mfa, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "mfa-user@torchwood.local",
		Password:  "User@123",
		Name:      "MFA User",
	})
	require.NoError(t, err)
	require.Nil(t, mfa)

	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})
	return userCtx, account, projectID, user.ID, mr
}

func TestAccount_ListFactors(t *testing.T) {
	ctx, account, _, _, _ := setupMFATestAccount(t)

	factors, err := account.ListFactors(ctx)
	require.NoError(t, err)
	require.Empty(t, factors)
}

func TestAccount_CreateTOTPFactor_SecretEncrypted(t *testing.T) {
	ctx, account, projectID, userID, _ := setupMFATestAccount(t)

	factor, plainSecret, otpauthURL, err := account.CreateTOTPFactor(ctx, projectID, userID, "")
	require.NoError(t, err)
	require.NotEmpty(t, plainSecret)
	require.Contains(t, otpauthURL, "otpauth://totp/")

	// 落库密文：非明文、带 enc:v1: 前缀。
	found, err := account.usersRepo.GetByID(ctx, projectID, userID)
	require.NoError(t, err)
	require.NotNil(t, found)
	rawFactors := parseFactorsRaw(found.Factors)
	require.NotEmpty(t, rawFactors)
	storedJSON := factorDocs(rawFactors)[0].(map[string]any)
	require.Contains(t, storedJSON["secret"], "enc:v1:")
	require.NotContains(t, storedJSON["secret"], plainSecret)

	// List 不含明文 secret。
	list, err := account.ListFactors(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, factor.ID, list[0].ID)
	require.Empty(t, list[0].Secret)
	require.Equal(t, auth.FactorStatusPending, list[0].Status)
}

func TestAccount_CreateTOTPFactor_RequiresJWTSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	projectRepo := bunrepo.NewProjectRepository(db)

	// 空 jwt secret 的配置。
	account := NewTestAccountWithRedis(&config.AppConfig{}, projectRepo, db, rdb)

	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "mfa-nosecret@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Roles:     []string{"users", "user:" + user.ID},
	})
	_, _, _, err = account.CreateTOTPFactor(userCtx, projectID, user.ID, "")
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Internal, st.Code())
}

func TestAccount_VerifyTOTPFactor_Activate(t *testing.T) {
	ctx, account, projectID, userID, _ := setupMFATestAccount(t)

	factor, plainSecret, _, err := account.CreateTOTPFactor(ctx, projectID, userID, "")
	require.NoError(t, err)

	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	verified, err := account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, code)
	require.NoError(t, err)
	require.Equal(t, auth.FactorStatusVerified, verified.Status)

	// 已 verified 因子不可重复激活。
	_, err = account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, code)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestAccount_VerifyTOTPFactor_WrongCode(t *testing.T) {
	ctx, account, projectID, userID, _ := setupMFATestAccount(t)

	factor, _, _, err := account.CreateTOTPFactor(ctx, projectID, userID, "")
	require.NoError(t, err)

	_, err = account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, "123456")
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())

	// 因子仍为 pending，可重试。
	factors, err := account.ListFactors(ctx)
	require.NoError(t, err)
	require.Equal(t, auth.FactorStatusPending, factors[0].Status)
}

func TestAccount_VerifyTOTPFactor_LockedAfterFailures(t *testing.T) {
	ctx, account, projectID, userID, _ := setupMFATestAccount(t)

	factor, plainSecret, _, err := account.CreateTOTPFactor(ctx, projectID, userID, "")
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		_, err := account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, "000000")
		require.Error(t, err)
	}
	// 正确 code 也被锁定拒绝。
	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	_, err = account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, code)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.ResourceExhausted, st.Code())
}

func TestAccount_DeleteFactor_RestoresDirectSignIn(t *testing.T) {
	ctx, account, projectID, userID, mr := setupMFATestAccount(t)

	factor, plainSecret, _, err := account.CreateTOTPFactor(ctx, projectID, userID, "")
	require.NoError(t, err)
	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	_, err = account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, code)
	require.NoError(t, err)

	// 删除 verified 因子需二次验证：无 code / 错误 code 均拒绝。
	require.Error(t, account.DeleteFactor(ctx, projectID, userID, factor.ID, ""))
	require.Error(t, account.DeleteFactor(ctx, projectID, userID, factor.ID, "123456"))

	// 等激活 code 的防重放窗口（60s）过后生成新 code。
	mr.FastForward(61 * time.Second)
	code2, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, account.DeleteFactor(ctx, projectID, userID, factor.ID, code2))

	factors, err := account.ListFactors(ctx)
	require.NoError(t, err)
	require.Empty(t, factors)

	// 删除后登录恢复直通（无 MFA 挑战）。
	_, _, _, mfa, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "mfa-user@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.Nil(t, mfa)
}

// TestAccount_DeleteFactor_RevokesPendingChallenges 删除因子时作废该用户
// 全部未消费的登录挑战。
func TestAccount_DeleteFactor_RevokesPendingChallenges(t *testing.T) {
	ctx, account, projectID, userID, mr := setupMFATestAccount(t)

	factor, plainSecret, _, err := account.CreateTOTPFactor(ctx, projectID, userID, "")
	require.NoError(t, err)
	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	_, err = account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, code)
	require.NoError(t, err)

	// 删除当前会话（SignUp 产生的），触发 MFA 登录挑战。
	sessions, err := account.ListSessions(ctx)
	require.NoError(t, err)
	for _, s := range sessions {
		require.NoError(t, account.DeleteSession(ctx, s.ID))
	}
	_, _, _, mfa, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "mfa-user@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotNil(t, mfa)
	token := mfa.Token

	// 删除因子（等激活 code 的防重放窗口过后生成新 code）。
	mr.FastForward(61 * time.Second)
	code2, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	require.NoError(t, account.DeleteFactor(ctx, projectID, userID, factor.ID, code2))

	// 未消费的挑战已作废。
	_, _, _, err = account.CompleteMFASession(ctx, projectID, token, factor.ID, code2)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

// TestAccount_SignInRequiresMFA 登录钩子：有 verified 因子时返回 mfa_required，
// 且不产生会话文档；CompleteMFASession 后会话可用；challenge 一次性。
func TestAccount_VerifyTOTPFactor_ExpiredPending(t *testing.T) {
	ctx, account, projectID, userID, _ := setupMFATestAccount(t)

	factor, _, _, err := account.CreateTOTPFactor(ctx, projectID, userID, "")
	require.NoError(t, err)

	// 把因子 created_at 改成 11 分钟前，模拟激活超时。
	old := factor.CreatedAt.Add(-11 * time.Minute)
	err = account.usersRepo.UpdateFactors(ctx, projectID, userID, func(current json.RawMessage) (json.RawMessage, error) {
		factors := parseFactorsRaw(current)
		for i := range factors {
			if factors[i].ID == factor.ID {
				factors[i].CreatedAt = old
			}
		}
		return json.Marshal(factorDocs(factors))
	})
	require.NoError(t, err)

	code, err := totp.GenerateCode("whatever", time.Now())
	require.NoError(t, err)
	_, err = account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, code)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())

	// 过期因子已被删除。
	list, err := account.ListFactors(ctx)
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestAccount_SignInRequiresMFA(t *testing.T) {
	ctx, account, projectID, userID, mr := setupMFATestAccount(t)

	factor, plainSecret, _, err := account.CreateTOTPFactor(ctx, projectID, userID, "")
	require.NoError(t, err)
	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	_, err = account.VerifyTOTPFactor(ctx, projectID, userID, factor.ID, code)
	require.NoError(t, err)

	// 等激活 code 的 60s 防重放窗口过后再走登录流程（新 code 可被接受）。
	mr.FastForward(61 * time.Second)

	// 登出当前会话（SignUp 产生的），确保登录前无活跃会话。
	sessions, err := account.ListSessions(ctx)
	require.NoError(t, err)
	for _, s := range sessions {
		require.NoError(t, account.DeleteSession(ctx, s.ID))
	}

	// SignIn 触发 MFA 挑战：不签发会话。
	user, tokens, cookie, mfa, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "mfa-user@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotNil(t, mfa)
	require.NotEmpty(t, mfa.Token)
	require.Len(t, mfa.Factors, 1)
	require.Nil(t, tokens)
	require.Empty(t, cookie)
	require.Equal(t, userID, user.ID)

	// 无新会话文档。
	sessionsAfter, err := account.ListSessions(ctx)
	require.NoError(t, err)
	require.Empty(t, sessionsAfter)

	// 错误 code 拒绝。
	_, _, _, err = account.CompleteMFASession(ctx, projectID, mfa.Token, factor.ID, "123456")
	require.Error(t, err)

	// 错误 code 后 challenge 已被消耗：正确 code 也无法再使用同一 token。
	goodCode, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	_, _, _, err = account.CompleteMFASession(ctx, projectID, mfa.Token, factor.ID, goodCode)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())
	require.Contains(t, strings.ToLower(err.Error()), "challenge")

	// 重新登录获取新挑战，用正确 code 完成挑战 → 签发会话。
	_, _, _, mfa2, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "mfa-user@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotNil(t, mfa2)
	goodCode2, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	user2, tokens2, cookie2, err := account.CompleteMFASession(ctx, projectID, mfa2.Token, factor.ID, goodCode2)
	require.NoError(t, err)
	require.Equal(t, userID, user2.ID)
	require.NotEmpty(t, tokens2.AccessToken)
	require.NotEmpty(t, cookie2)

	// 会话可用。
	meCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user2.ID,
		Email:     user2.Email,
		Roles:     []string{"users", "user:" + user2.ID},
	})
	me, err := account.Me(meCtx)
	require.NoError(t, err)
	require.Equal(t, userID, me.ID)
}

// GetByID 返回空 factors，UpdateFactors 却拿到并发写入的 current；
// mutate 必须基于锁定行，不得用锁前快照整袋覆盖。
func TestParseFactorsRaw_ObjectArray(t *testing.T) {
	got := parseFactorsRaw(json.RawMessage(`[{"id":"concurrent","type":"totp","secret":"c","status":"pending","created_at":"2026-01-01T00:00:00Z"}]`))
	require.Len(t, got, 1)
	require.Equal(t, "concurrent", got[0].ID)
}

func TestAccount_CreateTOTPFactor_MergesLockedCurrent(t *testing.T) {
	repo := &mergeFactorsRepo{current: factorsJSON(recentFactor("concurrent", auth.FactorStatusPending))}
	account := mergeTestAccount(repo)
	factor, _, _, err := account.CreateTOTPFactor(mergeUserCtx(), "p1", "u1", "")
	require.NoError(t, err)
	require.Equal(t, "new-1", factor.ID)
	require.Contains(t, string(repo.stored), `"concurrent"`)
	require.Contains(t, string(repo.stored), `"new-1"`)
}

func TestAccount_VerifyTOTPFactor_MergesLockedCurrent(t *testing.T) {
	repo := &mergeFactorsRepo{current: factorsJSON(
		recentFactor("concurrent", auth.FactorStatusVerified),
		recentFactor("new-1", auth.FactorStatusPending),
	)}
	account := mergeTestAccount(repo)
	verified, err := account.VerifyTOTPFactor(mergeUserCtx(), "p1", "u1", "new-1", "000000")
	require.NoError(t, err)
	require.Equal(t, "new-1", verified.ID)
	require.Equal(t, auth.FactorStatusVerified, verified.Status)
	require.Contains(t, string(repo.stored), `"concurrent"`)
	require.Contains(t, string(repo.stored), `"new-1"`)
	ids := parseFactorsRaw(repo.stored)
	require.Len(t, ids, 2)
	byID := map[string]auth.Factor{}
	for _, f := range ids {
		byID[f.ID] = f
	}
	require.Equal(t, auth.FactorStatusVerified, byID["concurrent"].Status)
	require.Equal(t, auth.FactorStatusVerified, byID["new-1"].Status)
}

func TestAccount_DeleteFactor_MergesLockedCurrent(t *testing.T) {
	repo := &mergeFactorsRepo{current: factorsJSON(
		recentFactor("concurrent", auth.FactorStatusPending),
		recentFactor("new-1", auth.FactorStatusPending),
	)}
	account := mergeTestAccount(repo)
	require.NoError(t, account.DeleteFactor(mergeUserCtx(), "p1", "u1", "new-1", ""))
	require.Contains(t, string(repo.stored), `"concurrent"`)
	require.NotContains(t, string(repo.stored), `"new-1"`)
}

func mergeTestAccount(repo *mergeFactorsRepo) *Account {
	return &Account{
		usersRepo:   repo,
		mfa:         stubMergeMFA{},
		projectRepo: stubProjects{},
	}
}

func mergeUserCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: "p1",
		UserID:    "u1",
	})
}

func recentFactor(id, status string) map[string]any {
	return map[string]any{
		"id":         id,
		"type":       auth.FactorTypeTOTP,
		"secret":     "c",
		"status":     status,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func factorsJSON(items ...map[string]any) json.RawMessage {
	raw, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return raw
}

type mergeFactorsRepo struct {
	current json.RawMessage
	stored  json.RawMessage
}

var _ users.Repository = (*mergeFactorsRepo)(nil)

func (r *mergeFactorsRepo) GetByEmail(context.Context, string, string) (*users.User, error) {
	return nil, nil
}
func (r *mergeFactorsRepo) GetByID(context.Context, string, string) (*users.User, error) {
	return &users.User{ID: "u1", Email: "a@b.c", Status: users.StatusActive, Factors: json.RawMessage(`[]`)}, nil
}
func (r *mergeFactorsRepo) GetByPhone(context.Context, string, string) (*users.User, error) {
	return nil, nil
}
func (r *mergeFactorsRepo) Insert(context.Context, string, *users.User) error { return nil }
func (r *mergeFactorsRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *mergeFactorsRepo) Delete(context.Context, string, string) error { return nil }
func (r *mergeFactorsRepo) List(context.Context, string, users.ListFilter) (*users.ListResult, error) {
	return &users.ListResult{}, nil
}
func (r *mergeFactorsRepo) UpdateFactors(_ context.Context, _, _ string, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	current := r.current
	if len(current) == 0 {
		current = json.RawMessage(`[]`)
	}
	next, err := mutate(current)
	if err != nil {
		return err
	}
	r.stored = next
	return nil
}

type stubMergeMFA struct{}

func (stubMergeMFA) CreateTOTPFactor(context.Context, string, string, string) (*auth.Factor, string, string, error) {
	return &auth.Factor{
		ID:        "new-1",
		Type:      auth.FactorTypeTOTP,
		Secret:    "enc:v1:x", // #nosec G101 -- 测试固定值
		Status:    auth.FactorStatusPending,
		CreatedAt: time.Now(),
	}, "plain", "otpauth://totp/x", nil
}
func (stubMergeMFA) VerifyTOTPFactor(context.Context, *auth.Factor, string) error { return nil }
func (stubMergeMFA) ValidateTOTP(context.Context, *auth.Factor, string) error     { return nil }
