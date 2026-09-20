package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appclient "github.com/torchwoodcloud/torchwood/internal/app/client"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domaingroups "github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件测试对外 token 校验用例（appserver.Auth.VerifyToken）。判定链路直接
// 走真实 infra/auth.Validator + 真实角色解析器（appclient.UserRoles，与生产
// 装配同构），断言 introspection 与进程内校验语义完全一致；repo 用内存桩
//（app/server 内部测试桩不跨包复用，此处自持最小实现）。

const verifyTestJWTSecret = "auth-verify-test-secret"

// --- 内存桩（仅覆盖被测路径） ---

type verifyUserRepo struct {
	mu    sync.Mutex
	users map[string]*domainusers.User
}

func newVerifyUserRepo() *verifyUserRepo {
	return &verifyUserRepo{users: map[string]*domainusers.User{}}
}

func (r *verifyUserRepo) seed(u *domainusers.User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *u
	r.users[u.ID] = &cp
}

func (r *verifyUserRepo) GetByID(_ context.Context, _, id string) (*domainusers.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u := r.users[id]; u != nil {
		cp := *u
		return &cp, nil
	}
	return nil, nil
}

func (r *verifyUserRepo) GetByEmail(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *verifyUserRepo) GetByPhone(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *verifyUserRepo) Insert(context.Context, string, *domainusers.User) error { return nil }
func (r *verifyUserRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *verifyUserRepo) Delete(context.Context, string, string) error { return nil }
func (r *verifyUserRepo) List(context.Context, string, domainusers.ListFilter) (*domainusers.ListResult, error) {
	return &domainusers.ListResult{}, nil
}
func (r *verifyUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return nil
}

type verifySessionRepo struct {
	mu       sync.Mutex
	sessions map[string]*domainauth.Session
}

func newVerifySessionRepo() *verifySessionRepo {
	return &verifySessionRepo{sessions: map[string]*domainauth.Session{}}
}

func (r *verifySessionRepo) seed(projectID string, s *domainauth.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *s
	r.sessions[projectID+"/"+s.ID] = &cp
}

func (r *verifySessionRepo) Insert(_ context.Context, projectID string, s *domainauth.Session) error {
	r.seed(projectID, s)
	return nil
}

func (r *verifySessionRepo) GetByID(_ context.Context, projectID, id string) (*domainauth.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[projectID+"/"+id]; s != nil {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (r *verifySessionRepo) Delete(_ context.Context, projectID, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, projectID+"/"+id)
	return nil
}

func (r *verifySessionRepo) ListByUser(context.Context, string, string) ([]domainauth.Session, error) {
	return nil, nil
}
func (r *verifySessionRepo) DeleteByUser(context.Context, string, string) error { return nil }
func (r *verifySessionRepo) DeleteOldestByUser(context.Context, string, string, int) error {
	return nil
}

type verifyMembershipRepo struct {
	mu   sync.Mutex
	rows map[string]*domaingroups.Membership
}

func newVerifyMembershipRepo() *verifyMembershipRepo {
	return &verifyMembershipRepo{rows: map[string]*domaingroups.Membership{}}
}

func (r *verifyMembershipRepo) seed(m *domaingroups.Membership) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *m
	r.rows[m.ID] = &cp
}

func (r *verifyMembershipRepo) ListByUser(_ context.Context, _ string, userID string) ([]*domaingroups.Membership, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domaingroups.Membership
	for _, m := range r.rows {
		if m.UserID == userID {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *verifyMembershipRepo) Insert(context.Context, string, *domaingroups.Membership) error {
	return nil
}
func (r *verifyMembershipRepo) GetByID(context.Context, string, string) (*domaingroups.Membership, error) {
	return nil, nil
}
func (r *verifyMembershipRepo) ListByGroup(context.Context, string, string) ([]*domaingroups.Membership, error) {
	return nil, nil
}
func (r *verifyMembershipRepo) Delete(context.Context, string, string, func(context.Context, *domaingroups.Membership) error) error {
	return nil
}
func (r *verifyMembershipRepo) Accept(context.Context, string, string, string, time.Time) error {
	return nil
}
func (r *verifyMembershipRepo) Reject(context.Context, string, string) error { return nil }
func (r *verifyMembershipRepo) UpdateRoles(context.Context, string, string, func(context.Context, *domaingroups.Membership) ([]string, error)) error {
	return nil
}

type verifyAPIKeyRepo struct {
	mu     sync.Mutex
	byHash map[string]*projects.APIKey
	byID   map[string]*projects.APIKey
}

func newVerifyAPIKeyRepo() *verifyAPIKeyRepo {
	return &verifyAPIKeyRepo{byHash: map[string]*projects.APIKey{}, byID: map[string]*projects.APIKey{}}
}

func (r *verifyAPIKeyRepo) seed(k *projects.APIKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *k
	r.byHash[k.SecretHash] = &cp
	r.byID[k.ID] = &cp
}

func (r *verifyAPIKeyRepo) CreateAPIKey(context.Context, *projects.APIKey) error { return nil }
func (r *verifyAPIKeyRepo) GetAPIKey(_ context.Context, _ string, id string) (*projects.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := r.byID[id]; k != nil {
		cp := *k
		return &cp, nil
	}
	return nil, nil
}
func (r *verifyAPIKeyRepo) GetAPIKeyBySecretHash(_ context.Context, hash string) (*projects.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := r.byHash[hash]; k != nil {
		cp := *k
		return &cp, nil
	}
	return nil, nil
}
func (r *verifyAPIKeyRepo) ListAPIKeys(context.Context, string) ([]projects.APIKey, error) {
	return nil, nil
}
func (r *verifyAPIKeyRepo) UpdateAPIKey(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *verifyAPIKeyRepo) DeleteAPIKey(context.Context, string, string) error { return nil }

// verifyOneTimeStore 是 domainauth.OneTimeTokenStore 内存桩（记录消费次数，
// 断言 introspection 不产生一次性 token 消费副作用）。
type verifyOneTimeStore struct {
	mu       sync.Mutex
	records  map[string]string
	consumed int
}

func newVerifyOneTimeStore() *verifyOneTimeStore {
	return &verifyOneTimeStore{records: map[string]string{}}
}

func (s *verifyOneTimeStore) Register(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[key]; ok {
		return false, nil
	}
	s.records[key] = value
	return true, nil
}

func (s *verifyOneTimeStore) Consume(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.records[key]
	if !ok {
		return "", nil
	}
	delete(s.records, key)
	s.consumed++
	return v, nil
}

var _ domainauth.OneTimeTokenStore = (*verifyOneTimeStore)(nil)

// --- 装配与测试 ---

// verifyFixture 组装被测用例与真实校验链路。
type verifyFixture struct {
	useCase      *appserver.Auth
	users        *verifyUserRepo
	sessions     *verifySessionRepo
	memberships  *verifyMembershipRepo
	apiKeys      *verifyAPIKeyRepo
	oneTimeStore *verifyOneTimeStore
	projectID    string
	userID       string
	sessionID    string
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	f := &verifyFixture{
		users:        newVerifyUserRepo(),
		sessions:     newVerifySessionRepo(),
		memberships:  newVerifyMembershipRepo(),
		apiKeys:      newVerifyAPIKeyRepo(),
		oneTimeStore: newVerifyOneTimeStore(),
		projectID:    "proj-verify",
		userID:       "user-verify-1",
		sessionID:    "sess-verify-1",
	}
	validator := auth.NewValidatorWithOneTimeTokens(
		&config.AppConfig{Security: &config.Security{Jwt: &config.Security_Jwt{Secret: verifyTestJWTSecret}}},
		f.apiKeys, nil, nil, nil, nil, f.sessions, f.users,
		appclient.NewUserRoles(f.users, f.memberships),
		f.oneTimeStore, nil,
	)
	f.useCase = appserver.NewAuth(validator, f.apiKeys, f.users, f.memberships)
	return f
}

func (f *verifyFixture) seedEndUser() {
	f.users.seed(&domainusers.User{
		ID:            f.userID,
		Email:         "bridge@example.com",
		Status:        domainusers.StatusActive,
		EmailVerified: true,
		Labels:        []string{"vip"},
	})
	f.sessions.seed(f.projectID, &domainauth.Session{
		ID:       f.sessionID,
		UserID:   f.userID,
		ExpireAt: time.Now().Add(time.Hour),
	})
	f.memberships.seed(&domaingroups.Membership{
		ID:      "mem-1",
		GroupID: "group-1",
		UserID:  f.userID,
		Roles:   []string{"owner"},
		Status:  domaingroups.StatusAccepted,
	})
}

// signVerifyJWT 按 claims.ActorKind 选择 purpose 派生密钥签发测试 JWT
// （与生产签发/验签同构）。
func signVerifyJWT(t *testing.T, claims jwtparser.Claims) string {
	t.Helper()
	purpose := jwtparser.PurposeEndUserJWT
	if claims.ActorKind == "admin" {
		purpose = jwtparser.PurposeAdminJWT
	}
	token, err := jwtparser.Generate(jwtparser.DeriveKey(verifyTestJWTSecret, purpose), claims)
	require.NoError(t, err)
	return token
}

func (f *verifyFixture) endUserClaims() jwtparser.Claims {
	return jwtparser.Claims{
		UserID:    f.userID,
		Username:  "bridge@example.com",
		ProjectID: f.projectID,
		SessionID: f.sessionID,
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}
}

// verifyCallerCtx 模拟持 users.read 的同项目 API key 调用方（桥接服务）。
func verifyCallerCtx(projectID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:        "caller-key",
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		ProjectID:      projectID,
		APIKeyID:       "caller-key",
	})
}

func requireInvalid(t *testing.T, res *appserver.VerifiedCredential, err error) {
	t.Helper()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.Valid)
	require.Empty(t, res.UserID)
	require.Empty(t, res.Roles)
}

// TestAuth_VerifyToken_EndUserJWT_Valid：合法端用户 access JWT → valid=true，
// 且 user/project/session/expires_at 与实时解析的 roles/groups/labels 齐全。
func TestAuth_VerifyToken_EndUserJWT_Valid(t *testing.T) {
	f := newVerifyFixture(t)
	f.seedEndUser()
	claims := f.endUserClaims()
	token := signVerifyJWT(t, claims)

	res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: token})
	require.NoError(t, err)
	require.True(t, res.Valid)
	require.Equal(t, f.userID, res.UserID)
	require.Equal(t, "bridge@example.com", res.Username)
	require.Equal(t, f.projectID, res.ProjectID)
	require.Equal(t, f.sessionID, res.SessionID)
	require.Equal(t, time.Unix(claims.ExpiresAt, 0).UTC(), res.ExpiresAt)
	require.Equal(t, shared.ActorKindEndUser, res.ActorKind)
	require.Empty(t, res.APIKeyID)
	// 角色词汇实时解析（不信任 JWT rls claim——本 claims 根本未携带 rls）。
	require.ElementsMatch(t, []string{
		"users",
		"user:" + f.userID,
		"user:" + f.userID + "/verified",
		"label:vip",
		"group:group-1",
		"member:mem-1",
		"group:group-1/owner",
	}, res.Roles)
	require.Equal(t, []appserver.VerifiedGroupRef{{ID: "group-1", Role: "owner"}}, res.Groups)
	require.Equal(t, []string{"vip"}, res.Labels)
}

// TestAuth_VerifyToken_EndUserJWT_StaleRolesClaimIgnored：JWT rls claim 携带
// 过期角色，判定仍以实时解析为准（校验语义与进程内一致）。
func TestAuth_VerifyToken_EndUserJWT_StaleRolesClaimIgnored(t *testing.T) {
	f := newVerifyFixture(t)
	f.seedEndUser()
	claims := f.endUserClaims()
	claims.Roles = []string{"group:ghost-group", "label:ghost"}
	token := signVerifyJWT(t, claims)

	res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: token})
	require.NoError(t, err)
	require.True(t, res.Valid)
	require.NotContains(t, res.Roles, "group:ghost-group")
	require.NotContains(t, res.Roles, "label:ghost")
}

// TestAuth_VerifyToken_EndUserJWT_Invalidated：会话删除 / 会话过期 / 用户
// 封禁 / JWT 过期 / 用户不存在 → valid=false + 无错误（200 语义）。
func TestAuth_VerifyToken_EndUserJWT_Invalidated(t *testing.T) {
	t.Run("session deleted", func(t *testing.T) {
		f := newVerifyFixture(t)
		f.seedEndUser()
		require.NoError(t, f.sessions.Delete(context.Background(), f.projectID, f.sessionID))
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: signVerifyJWT(t, f.endUserClaims())})
		requireInvalid(t, res, err)
	})
	t.Run("session expired", func(t *testing.T) {
		f := newVerifyFixture(t)
		f.seedEndUser()
		f.sessions.seed(f.projectID, &domainauth.Session{
			ID: f.sessionID, UserID: f.userID, ExpireAt: time.Now().Add(-time.Minute),
		})
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: signVerifyJWT(t, f.endUserClaims())})
		requireInvalid(t, res, err)
	})
	t.Run("user blocked", func(t *testing.T) {
		f := newVerifyFixture(t)
		f.seedEndUser()
		f.users.seed(&domainusers.User{ID: f.userID, Email: "bridge@example.com", Status: domainusers.StatusBlocked})
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: signVerifyJWT(t, f.endUserClaims())})
		requireInvalid(t, res, err)
	})
	t.Run("expired jwt", func(t *testing.T) {
		f := newVerifyFixture(t)
		f.seedEndUser()
		claims := f.endUserClaims()
		claims.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: signVerifyJWT(t, claims)})
		requireInvalid(t, res, err)
	})
	t.Run("unknown user", func(t *testing.T) {
		f := newVerifyFixture(t)
		f.seedEndUser()
		claims := f.endUserClaims()
		claims.UserID = "user-not-exist"
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: signVerifyJWT(t, claims)})
		requireInvalid(t, res, err)
	})
}

// TestAuth_VerifyToken_RejectsNonEndUserCredentials：admin JWT / refresh
// token / 一次性 JWT / 函数执行 token / 垃圾串一律 valid=false；一次性
// token 只读判定、绝不消费（无副作用）。
func TestAuth_VerifyToken_RejectsNonEndUserCredentials(t *testing.T) {
	f := newVerifyFixture(t)
	f.seedEndUser()

	t.Run("admin jwt", func(t *testing.T) {
		token := signVerifyJWT(t, jwtparser.Claims{
			UserID:    "admin-1",
			ActorKind: "admin",
			TokenType: jwtparser.TokenTypeAccess,
			IssuedAt:  time.Now().Unix(),
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: token})
		requireInvalid(t, res, err)
	})
	t.Run("refresh token", func(t *testing.T) {
		claims := f.endUserClaims()
		claims.TokenType = jwtparser.TokenTypeRefresh
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: signVerifyJWT(t, claims)})
		requireInvalid(t, res, err)
	})
	t.Run("one-time jwt is not consumed", func(t *testing.T) {
		claims := f.endUserClaims()
		claims.OneTime = true
		claims.TokenID = "ott-1"
		_, err := f.oneTimeStore.Register(context.Background(), domainauth.OneTimeJWTKeyPrefix+claims.TokenID, f.userID, time.Hour)
		require.NoError(t, err)
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: signVerifyJWT(t, claims)})
		requireInvalid(t, res, err)
		require.Equal(t, 0, f.oneTimeStore.consumed, "introspection 不得消费一次性 token")
	})
	t.Run("garbage and execution token", func(t *testing.T) {
		for _, token := range []string{"not-a-jwt", "twx-execution-token"} {
			res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: token})
			requireInvalid(t, res, err)
		}
	})
}

// TestAuth_VerifyToken_APIKey：合法 key → valid=true、actor_kind=service、
// api_key_id/project_id/expires_at 正确；禁用/过期/不存在 → valid=false。
func TestAuth_VerifyToken_APIKey(t *testing.T) {
	f := newVerifyFixture(t)
	secret := "sk-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaabbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	expireAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	seedKey := func(t *testing.T, mod func(k *projects.APIKey)) {
		k := &projects.APIKey{
			ID:         "key-verify-1",
			ProjectID:  f.projectID,
			SecretHash: hashVerifySecret(secret),
			Scopes:     []string{"users.read"},
			Enabled:    true,
			ExpireAt:   &expireAt,
		}
		if mod != nil {
			mod(k)
		}
		f.apiKeys.seed(k)
	}

	t.Run("valid auto dispatch", func(t *testing.T) {
		seedKey(t, nil)
		// AUTO 分派：sk- 前缀走 API key 通道。
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: secret})
		require.NoError(t, err)
		require.True(t, res.Valid)
		require.Equal(t, shared.ActorKindService, res.ActorKind)
		require.Equal(t, "key-verify-1", res.APIKeyID)
		require.Equal(t, f.projectID, res.ProjectID)
		require.Equal(t, expireAt, res.ExpiresAt)
		require.Equal(t, []string{"keys", "key:key-verify-1"}, res.Roles)
		require.Empty(t, res.UserID)
		require.Empty(t, res.SessionID)
	})
	t.Run("valid explicit type", func(t *testing.T) {
		seedKey(t, nil)
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: secret, Type: appserver.CredentialAPIKey})
		require.NoError(t, err)
		require.True(t, res.Valid)
	})
	t.Run("disabled", func(t *testing.T) {
		seedKey(t, func(k *projects.APIKey) { k.Enabled = false })
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: secret})
		requireInvalid(t, res, err)
	})
	t.Run("expired", func(t *testing.T) {
		past := time.Now().Add(-time.Hour)
		seedKey(t, func(k *projects.APIKey) { k.ExpireAt = &past })
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: secret})
		requireInvalid(t, res, err)
	})
	t.Run("unknown", func(t *testing.T) {
		res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: "sk-unknown"})
		requireInvalid(t, res, err)
	})
}

// TestAuth_VerifyToken_TypeMismatch：显式类型与凭证结构不符 → valid=false
// （不强猜：TOKEN 类型收到 sk- 串、API_KEY 类型收到 JWT 均拒绝）。
func TestAuth_VerifyToken_TypeMismatch(t *testing.T) {
	f := newVerifyFixture(t)
	f.seedEndUser()
	token := signVerifyJWT(t, f.endUserClaims())

	res, err := f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: "sk-whatever", Type: appserver.CredentialJWT})
	requireInvalid(t, res, err)
	res, err = f.useCase.VerifyToken(verifyCallerCtx(f.projectID), appserver.VerifyTokenCommand{Token: token, Type: appserver.CredentialAPIKey})
	requireInvalid(t, res, err)
}

// TestAuth_VerifyToken_CrossProjectIsolation：调用者与凭证分属不同项目 →
// valid=false（不泄露他项目主体信息）；平台 admin（无项目上下文）放行。
func TestAuth_VerifyToken_CrossProjectIsolation(t *testing.T) {
	f := newVerifyFixture(t)
	f.seedEndUser()
	token := signVerifyJWT(t, f.endUserClaims())

	res, err := f.useCase.VerifyToken(verifyCallerCtx("proj-other"), appserver.VerifyTokenCommand{Token: token})
	requireInvalid(t, res, err)

	// 平台 admin 未选项目（ProjectID 空）→ 可跨项目校验。
	platformCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: true,
	})
	res, err = f.useCase.VerifyToken(platformCtx, appserver.VerifyTokenCommand{Token: token})
	require.NoError(t, err)
	require.True(t, res.Valid)
}

// TestAuth_VerifyToken_RequiresServerPrincipal：纵深防御——匿名 →
// Unauthenticated；端用户 → PermissionDenied。
func TestAuth_VerifyToken_RequiresServerPrincipal(t *testing.T) {
	f := newVerifyFixture(t)
	f.seedEndUser()
	token := signVerifyJWT(t, f.endUserClaims())

	_, err := f.useCase.VerifyToken(context.Background(), appserver.VerifyTokenCommand{Token: token})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	endUser := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindEndUser, UserID: f.userID,
	})
	_, err = f.useCase.VerifyToken(endUser, appserver.VerifyTokenCommand{Token: token})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func hashVerifySecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
