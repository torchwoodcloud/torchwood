package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"github.com/torchwoodcloud/torchwood/pkg/password"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// usersUCJWTSecret 是本文件集成测试用的固定 JWT 主密钥。
const usersUCJWTSecret = "users-integration-test-secret"

type documentRoles struct{}

func (documentRoles) LoadUserRoles(ctx context.Context, projectID, userID string, _ *users.User) ([]string, error) {
	return []string{"users", "user:" + userID}, nil
}

func newUsersUC(ctx context.Context, t *testing.T) (*Users, *clients.Database, string, func()) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	cfg := &config.AppConfig{Security: &config.Security{Jwt: &config.Security_Jwt{Secret: usersUCJWTSecret}}}
	sessions := auth.NewSessionService(cfg, bunrepo.NewSessionRepository(db), documentRoles{}, nil)
	uc := NewUsers(bunrepo.NewProjectRepository(db), sessions, db, bunrepo.NewUserRepository(db), bunrepo.NewSessionRepository(db), bunrepo.NewGroupRepository(db), bunrepo.NewMembershipRepository(db), nil)
	return uc, db, projectID, cleanup
}

func TestServerUsers_CreateAndUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	uc, db, projectID, cleanup := newUsersUC(ctx, t)
	defer cleanup()

	// 创建用户。
	doc, err := uc.CreateUser(ctx, projectID, CreateUserCommand{
		Email:    "ServerUser@Test.Torchwood.local",
		Password: "Pass@123",
		Name:     "Server User",
		Labels:   []any{"vip", "beta"},
		Prefs:    map[string]any{"theme": "dark"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, doc.ID)
	require.Equal(t, "serveruser@test.torchwood.local", doc.Data["email"])
	require.Equal(t, users.StatusActive, doc.Data["status"])
	require.Equal(t, false, doc.Data["email_verified"])
	require.Equal(t, []any{"vip", "beta"}, doc.Data["labels"])
	require.Equal(t, map[string]any{"theme": "dark"}, doc.Data["prefs"])

	// 密码已哈希存储（repo 直读验证）；投影响应面不含 password_hash。
	require.NotContains(t, doc.Data, "password_hash")
	stored, err := bunrepo.NewUserRepository(db).GetByID(ctx, projectID, doc.ID)
	require.NoError(t, err)
	ok, _ := password.Verify("Pass@123", stored.PasswordHash)
	require.True(t, ok)

	// 重复 email。
	_, err = uc.CreateUser(ctx, projectID, CreateUserCommand{
		Email:    "serveruser@test.torchwood.local",
		Password: "Pass@123",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.AlreadyExists, st.Code())

	// 弱密码。
	_, err = uc.CreateUser(ctx, projectID, CreateUserCommand{
		Email:    "weak@torchwood.local",
		Password: "short",
	})
	require.Error(t, err)
	st, _ = status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())

	// 更新 status（此前被 protected 字段过滤，回归验证）。
	updated, err := uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"status": users.StatusBlocked}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Equal(t, users.StatusBlocked, updated.Data["status"])

	// 更新 email → 自动重置 email_verified，并校验唯一性。
	updated, err = uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"email": "new-email@torchwood.local"}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Equal(t, "new-email@torchwood.local", updated.Data["email"])
	require.Equal(t, false, updated.Data["email_verified"])

	// email_verified 可单独设置。
	updated, err = uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"email_verified": true}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Equal(t, true, updated.Data["email_verified"])

	// password_hash 仍受保护，不允许直接写。
	_, err = uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"password_hash": "evil"}, databases.Principal{Roles: []string{"keys"}})
	require.Error(t, err)
	st, _ = status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestServerUsers_PasswordResetAndSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	uc, db, projectID, cleanup := newUsersUC(ctx, t)
	defer cleanup()

	doc, err := uc.CreateUser(ctx, projectID, CreateUserCommand{
		Email:    "reset-test@torchwood.local",
		Password: "Pass@123",
		Name:     "Reset Test",
	})
	require.NoError(t, err)

	// 重置密码。
	_, err = uc.UpdateUserPassword(ctx, projectID, doc.ID, "NewPass@456")
	require.NoError(t, err)

	// DB 中当前哈希：旧密码失效、新密码可验证（repo 直读；投影不含 hash）。
	after, err := uc.GetUser(ctx, projectID, doc.ID, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.NotContains(t, after.Data, "password_hash")
	stored, err := bunrepo.NewUserRepository(db).GetByID(ctx, projectID, doc.ID)
	require.NoError(t, err)
	ok, _ := password.Verify("Pass@123", stored.PasswordHash)
	require.False(t, ok)
	ok, _ = password.Verify("NewPass@456", stored.PasswordHash)
	require.True(t, ok)

	// 弱密码拒绝。
	_, err = uc.UpdateUserPassword(ctx, projectID, doc.ID, "weak")
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())

	// 会话管理：先造两个会话，再列表/删除。
	s1, _, err := uc.sessions.CreateSessionAndTokens(ctx, projectID, doc.ID, "reset-test@torchwood.local", "email")
	require.NoError(t, err)
	_ = s1
	s2, _, err := uc.sessions.CreateSessionAndTokens(ctx, projectID, doc.ID, "reset-test@torchwood.local", "server_token")
	require.NoError(t, err)
	_ = s2

	sessionsList, err := uc.ListUserSessions(ctx, projectID, doc.ID)
	require.NoError(t, err)
	require.Len(t, sessionsList, 2)

	// 删除一个会话。
	target := sessionsList[0].ID
	require.NoError(t, uc.DeleteUserSession(ctx, projectID, doc.ID, target))
	sessionsList, err = uc.ListUserSessions(ctx, projectID, doc.ID)
	require.NoError(t, err)
	require.Len(t, sessionsList, 1)
	for _, s := range sessionsList {
		require.NotEqual(t, target, s.ID)
	}

	// 删除他人会话 → NotFound。
	other, err := uc.CreateUser(ctx, projectID, CreateUserCommand{
		Email:    "other@torchwood.local",
		Password: "Pass@123",
	})
	require.NoError(t, err)
	otherSession, _, err := uc.sessions.CreateSessionAndTokens(ctx, projectID, other.ID, "other@torchwood.local", "email")
	require.NoError(t, err)
	_ = otherSession
	err = uc.DeleteUserSession(ctx, projectID, other.ID, target)
	require.Error(t, err)
	st, _ = status.FromError(err)
	require.Equal(t, codes.NotFound, st.Code())

	// 不存在的用户 → NotFound。
	_, err = uc.ListUserSessions(ctx, projectID, "missing-user")
	require.Error(t, err)
	st, _ = status.FromError(err)
	require.Equal(t, codes.NotFound, st.Code())
}

// M5 C3（评审补偿控制）：UpdateUser 改 email/status 即撤销该用户全部端会话，
// 旧 session 验证失败；改其他字段（name）与同值幂等写不撤。
func TestServerUsers_UpdateUser_TakeoverFieldChangeRevokesSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	uc, _, projectID, cleanup := newUsersUC(ctx, t)
	defer cleanup()

	doc, err := uc.CreateUser(ctx, projectID, CreateUserCommand{
		Email: "takeover-test@torchwood.local", Password: "Pass@123",
	})
	require.NoError(t, err)
	newSession := func() string {
		bundle, _, err := uc.sessions.CreateSessionAndTokens(ctx, projectID, doc.ID, "takeover-test@torchwood.local", "email")
		require.NoError(t, err)
		return sessionIDFromBundle(t, bundle.AccessToken)
	}

	// 同值幂等：写入当前 email 不撤会话。
	sessID := newSession()
	_, err = uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"email": "takeover-test@torchwood.local"}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.NoError(t, uc.sessions.EnsureActiveSession(ctx, projectID, sessID, doc.ID))

	// 改 email：旧 session 验证失败（DeleteSessionsByUser 先撤后提交）。
	_, err = uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"email": "takeover-moved@torchwood.local"}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Error(t, uc.sessions.EnsureActiveSession(ctx, projectID, sessID, doc.ID), "改 email 后旧 session 必须已撤销")

	// 改 status：同样撤会话。
	sessID = newSession()
	_, err = uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"status": users.StatusBlocked}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Error(t, uc.sessions.EnsureActiveSession(ctx, projectID, sessID, doc.ID), "改 status 后旧 session 必须已撤销")

	// 改非接管面字段（name）：不撤会话。
	sessID = newSession()
	_, err = uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"name": "Renamed"}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.NoError(t, uc.sessions.EnsureActiveSession(ctx, projectID, sessID, doc.ID), "改 name 不得撤会话")
}

// sessionIDFromBundle 解析 access token 得到会话 ID。
func sessionIDFromBundle(t *testing.T, accessToken string) string {
	t.Helper()
	claims, ok := jwtparser.Parse(jwtparser.DeriveKey(usersUCJWTSecret, jwtparser.PurposeEndUserJWT), accessToken)
	require.True(t, ok)
	return claims.SessionID
}

func TestServerUsers_DeleteUser_CascadeBeyondDefaultPage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// 需要本地 Postgres（CI 兜底）：级联删除必须循环分页，
	// 超过默认 50 条页上限的会话/成员不得残留（F4-1）。
	ctx := platformAdminCtx(context.Background())
	uc, db, projectID, cleanup := newUsersUC(ctx, t)
	defer cleanup()

	doc, err := uc.CreateUser(ctx, projectID, CreateUserCommand{
		Email:    "cascade-61@torchwood.local",
		Password: "Pass@123",
		Name:     "Cascade 61",
	})
	require.NoError(t, err)

	sessionsRepo := bunrepo.NewSessionRepository(db)
	groupsRepo := bunrepo.NewGroupRepository(db)
	membershipsRepo := bunrepo.NewMembershipRepository(db)
	for i := 0; i < 61; i++ {
		require.NoError(t, sessionsRepo.Insert(ctx, projectID, &domainauth.Session{
			ID:       idgen.UUID().String(),
			UserID:   doc.ID,
			Provider: "email",
			ExpireAt: time.Now().Add(time.Hour),
		}))
	}
	for i := 0; i < 3; i++ {
		g := &groups.Group{ID: idgen.UUID().String(), Name: fmt.Sprintf("g-%d", i)}
		require.NoError(t, groupsRepo.Insert(ctx, projectID, g))
		require.NoError(t, membershipsRepo.Insert(ctx, projectID, &groups.Membership{
			ID:      idgen.UUID().String(),
			GroupID: g.ID,
			UserID:  doc.ID,
			Status:  groups.StatusAccepted,
			Roles:   []string{groups.RoleMember},
		}))
	}

	require.NoError(t, uc.DeleteUser(ctx, projectID, doc.ID, databases.Principal{Roles: []string{"keys"}}))

	gone, err := bunrepo.NewUserRepository(db).GetByID(ctx, projectID, doc.ID)
	require.NoError(t, err)
	require.Nil(t, gone)
	left, err := sessionsRepo.ListByUser(ctx, projectID, doc.ID)
	require.NoError(t, err)
	require.Empty(t, left)
	mems, err := membershipsRepo.ListByUser(ctx, projectID, doc.ID)
	require.NoError(t, err)
	require.Empty(t, mems)
}

func TestServerUsers_CreateUserToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	uc, db, projectID, cleanup := newUsersUC(ctx, t)
	defer cleanup()

	doc, err := uc.CreateUser(ctx, projectID, CreateUserCommand{
		Email:    "token-test@torchwood.local",
		Password: "Pass@123",
		Name:     "Token Test",
	})
	require.NoError(t, err)

	// 模拟登录签发 token（M5 C2：短时会话，exp-iat 不得超过 1h 硬上限）。
	bundle, err := uc.CreateUserToken(ctx, projectID, doc.ID)
	require.NoError(t, err)
	require.NotEmpty(t, bundle.AccessToken)
	require.NotEmpty(t, bundle.RefreshToken)

	hardCap := int64(auth.CreateUserTokenMaxSessionTTL.Seconds())
	accessClaims, ok := jwtparser.Parse(jwtparser.DeriveKey(usersUCJWTSecret, jwtparser.PurposeEndUserJWT), bundle.AccessToken)
	require.True(t, ok)
	require.LessOrEqual(t, accessClaims.ExpiresAt-accessClaims.IssuedAt, hardCap)
	refreshClaims, ok := jwtparser.Parse(jwtparser.DeriveKey(usersUCJWTSecret, jwtparser.PurposeEndUserJWT), bundle.RefreshToken)
	require.True(t, ok)
	require.LessOrEqual(t, refreshClaims.ExpiresAt-refreshClaims.IssuedAt, hardCap)

	// 签发后产生一条该用户的会话（可被管理员列出/删除）。
	sessionsList, err := uc.ListUserSessions(ctx, projectID, doc.ID)
	require.NoError(t, err)
	require.Len(t, sessionsList, 1)

	me, err := bunrepo.NewUserRepository(db).GetByID(ctx, projectID, doc.ID)
	require.NoError(t, err)
	require.NotNil(t, me)

	// blocked 用户不可模拟登录。
	_, err = uc.UpdateUser(ctx, projectID, doc.ID, map[string]any{"status": users.StatusBlocked}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	_, err = uc.CreateUserToken(ctx, projectID, doc.ID)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.FailedPrecondition, st.Code())
}
