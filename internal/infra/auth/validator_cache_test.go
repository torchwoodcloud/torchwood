package auth_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth/principalcache"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
)

// countingUserRepo 包裹 stubUserRepo 统计 GetByID 调用次数（P0.5 清账断言：
// 缓存命中路径 0 次、首次路径恰好 1 次——LoadUserRoles 重复查询修复）。
type countingUserRepo struct {
	*stubUserRepo
	getByID atomic.Int64
}

func (r *countingUserRepo) GetByID(ctx context.Context, projectID, id string) (*domainusers.User, error) {
	r.getByID.Add(1)
	return r.stubUserRepo.GetByID(ctx, projectID, id)
}

// countingSessionRepo 包裹 stubSessionRepo 统计 GetByID 调用次数。
type countingSessionRepo struct {
	*stubSessionRepo
	getByID atomic.Int64
}

func (r *countingSessionRepo) GetByID(ctx context.Context, projectID, id string) (*domainauth.Session, error) {
	r.getByID.Add(1)
	return r.stubSessionRepo.GetByID(ctx, projectID, id)
}

// countingRoles 记录 LoadUserRoles 调用与是否携带已取回的 user 行。
type countingRoles struct {
	calls    atomic.Int64
	sawUser  atomic.Bool
	lastUser atomic.Value // *domainusers.User
}

func (r *countingRoles) LoadUserRoles(_ context.Context, _, userID string, u *domainusers.User) ([]string, error) {
	r.calls.Add(1)
	if u != nil {
		r.sawUser.Store(true)
		r.lastUser.Store(u)
	}
	return []string{databases.RoleUsers, databases.RoleUser(userID)}, nil
}

// newCachedValidator 组装带 principal 缓存的校验器与计数桩。
func newCachedValidator(t *testing.T) (*auth.Validator, *countingUserRepo, *countingSessionRepo, *countingRoles) {
	t.Helper()
	usersRepo := &countingUserRepo{stubUserRepo: newStubUserRepo()}
	sessionsRepo := &countingSessionRepo{stubSessionRepo: newStubSessionRepo()}
	roles := &countingRoles{}
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, sessionsRepo, usersRepo, roles)
	v.SetPrincipalCache(principalcache.New(nil))
	return v, usersRepo, sessionsRepo, roles
}

// TestValidator_PrincipalCacheHitSavesDBRoundtrips 缓存命中时 4 次 DB 往返
// → 0 次（1 次 Redis 标记检查在 nil-rdb 下省略）；首次路径恰好各 1 次且
// LoadUserRoles 收到已取回的 user 行（重复查询修复）。
func TestValidator_PrincipalCacheHitSavesDBRoundtrips(t *testing.T) {
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	sessionID := idgen.UUID().String()

	v, usersRepo, sessionsRepo, roles := newCachedValidator(t)
	usersRepo.seed(projectID, activeUser(userID))
	sessionsRepo.seed(projectID, &domainauth.Session{ID: sessionID, UserID: userID, ExpireAt: time.Now().Add(time.Hour)})

	token := signToken(t, jwtparser.Claims{
		UserID:    userID,
		ProjectID: projectID,
		SessionID: sessionID,
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	// 首次：实时校验（session 1 次 + user 1 次 + roles 1 次——修复后
	// LoadUserRoles 不再重复查 users）。
	p, err := v.ValidateToken(ctx, token)
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindEndUser, p.ActorKind)
	require.EqualValues(t, 1, usersRepo.getByID.Load(), "首次校验 users.GetByID 恰好 1 次（重复查询修复）")
	require.EqualValues(t, 1, sessionsRepo.getByID.Load())
	require.EqualValues(t, 1, roles.calls.Load())
	require.True(t, roles.sawUser.Load(), "角色解析必须收到已取回的 user 行")

	// 第二次：命中缓存，全部 DB 往返为 0。
	_, err = v.ValidateToken(ctx, token)
	require.NoError(t, err)
	require.EqualValues(t, 1, usersRepo.getByID.Load(), "缓存命中不得再查 users")
	require.EqualValues(t, 1, sessionsRepo.getByID.Load(), "缓存命中不得再查 sessions")
	require.EqualValues(t, 1, roles.calls.Load(), "缓存命中不得再解析角色")

	// 第三次：同样命中（稳态）。
	_, err = v.ValidateToken(ctx, token)
	require.NoError(t, err)
	require.EqualValues(t, 1, usersRepo.getByID.Load())
}

// TestValidator_PrincipalCacheInvalidateOnLogout 登出（DeleteSessionsByUser
// 咽喉）写失效标记 → 后续校验回退 DB 实时校验。
func TestValidator_PrincipalCacheInvalidateOnLogout(t *testing.T) {
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	sessionID := idgen.UUID().String()

	usersRepo := &countingUserRepo{stubUserRepo: newStubUserRepo()}
	sessionsRepo := &countingSessionRepo{stubSessionRepo: newStubSessionRepo()}
	roles := &countingRoles{}
	usersRepo.seed(projectID, activeUser(userID))
	sessionsRepo.seed(projectID, &domainauth.Session{ID: sessionID, UserID: userID, ExpireAt: time.Now().Add(time.Hour)})
	cfg := testValidatorConfig()
	v := auth.NewValidator(cfg, &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, sessionsRepo, usersRepo, roles)
	cache := principalcache.New(nil)
	v.SetPrincipalCache(cache)

	token := signToken(t, jwtparser.Claims{
		UserID:    userID,
		ProjectID: projectID,
		SessionID: sessionID,
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	_, err := v.ValidateToken(ctx, token)
	require.NoError(t, err)
	require.EqualValues(t, 1, usersRepo.getByID.Load())

	// 登出咽喉：缓存主动失效（本地条目删除；nil rdb 下无标记面）。
	require.NoError(t, cache.InvalidateUser(ctx, projectID, userID))
	_, err = v.ValidateToken(ctx, token)
	require.NoError(t, err, "登出失效后回退 DB 实时校验（stub 会话仍在）")
	require.EqualValues(t, 2, usersRepo.getByID.Load(), "失效后必须重新实时校验")
}
