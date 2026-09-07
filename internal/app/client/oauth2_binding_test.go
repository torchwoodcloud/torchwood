package client

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
)

// M5 C5（评审补偿控制）三支路单测：login nonce 成对、link 本人、link 冒名拒绝。

const oauthBindingTestProject = "proj-oauth-binding"

// ---- 桩 ----

type memOAuthStateStore struct {
	states map[string]domainauth.OAuthState
}

func newMemOAuthStateStore() *memOAuthStateStore {
	return &memOAuthStateStore{states: map[string]domainauth.OAuthState{}}
}

func (s *memOAuthStateStore) Save(_ context.Context, state domainauth.OAuthState, _ time.Duration) error {
	s.states[state.StateID] = state
	return nil
}

func (s *memOAuthStateStore) Consume(_ context.Context, stateID string) (*domainauth.OAuthState, error) {
	st, ok := s.states[stateID]
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired oauth state")
	}
	delete(s.states, stateID)
	return &st, nil
}

type stubOAuthFactory struct{}

func (stubOAuthFactory) NewAuthenticator(_, _, _, _ string, _ []string) (domainauth.OAuthAuthenticator, error) {
	return stubOAuthAuthenticator{}, nil
}

type stubOAuthAuthenticator struct{}

func (stubOAuthAuthenticator) AuthorizeURL(stateID, _ string) string {
	return "https://provider.example/authorize?state=" + stateID
}

func (stubOAuthAuthenticator) Exchange(_ context.Context, code, _ string) (*domainauth.OAuthUserInfo, error) {
	return &domainauth.OAuthUserInfo{ProviderUID: "uid-" + code, Email: "oauth-" + code + "@example.com", EmailVerified: true}, nil
}

type stubOAuthProjectRepo struct{}

func (stubOAuthProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	return &projects.Project{ID: id, Status: "active", Settings: map[string]any{
		projects.SettingsKeyOAuthAllowedRedirectURLs: []any{"https://app.example/callback"},
	}}, nil
}
func (stubOAuthProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (stubOAuthProjectRepo) ListProjects(context.Context) ([]projects.Project, error) {
	return nil, nil
}
func (stubOAuthProjectRepo) CreateProject(context.Context, *projects.Project) error      { return nil }
func (stubOAuthProjectRepo) UpdateProject(context.Context, *projects.Project) error      { return nil }
func (stubOAuthProjectRepo) DeleteProject(context.Context, string) error                 { return nil }
func (stubOAuthProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }

type stubOAuthProviderRepo struct{}

func (stubOAuthProviderRepo) GetOAuthProvider(_ context.Context, _, _ string) (*projects.OAuthProvider, error) {
	return &projects.OAuthProvider{Enabled: true, ClientID: "cid", ClientSecret: "csec"}, nil
}
func (stubOAuthProviderRepo) ListOAuthProviders(context.Context, string) ([]projects.OAuthProvider, error) {
	return nil, nil
}
func (stubOAuthProviderRepo) UpsertOAuthProvider(context.Context, *projects.OAuthProvider) error {
	return nil
}
func (stubOAuthProviderRepo) DeleteOAuthProvider(context.Context, string, string) error { return nil }

type memUserRepo struct {
	byID map[string]*users.User
}

func (r *memUserRepo) GetByEmail(context.Context, string, string) (*users.User, error) {
	return nil, nil
}
func (r *memUserRepo) GetByID(_ context.Context, _, id string) (*users.User, error) {
	return r.byID[id], nil
}
func (r *memUserRepo) GetByPhone(context.Context, string, string) (*users.User, error) {
	return nil, nil
}
func (r *memUserRepo) Insert(_ context.Context, _ string, u *users.User) error {
	r.byID[u.ID] = u
	return nil
}
func (r *memUserRepo) Update(context.Context, string, string, map[string]any) error { return nil }
func (r *memUserRepo) Delete(context.Context, string, string) error                 { return nil }
func (r *memUserRepo) List(context.Context, string, users.ListFilter) (*users.ListResult, error) {
	return &users.ListResult{}, nil
}
func (r *memUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return nil
}

type memIdentityRepo struct {
	byUID map[string]*domainauth.Identity
}

func (r *memIdentityRepo) Insert(_ context.Context, _ string, i *domainauth.Identity) error {
	r.byUID[i.Provider+":"+i.ProviderUID] = i
	return nil
}
func (r *memIdentityRepo) GetByID(context.Context, string, string) (*domainauth.Identity, error) {
	return nil, nil
}
func (r *memIdentityRepo) GetByProviderUID(_ context.Context, _, provider, uid string) (*domainauth.Identity, error) {
	return r.byUID[provider+":"+uid], nil
}
func (r *memIdentityRepo) ListByUser(context.Context, string, string) ([]*domainauth.Identity, error) {
	return nil, nil
}
func (r *memIdentityRepo) Delete(context.Context, string, string) error { return nil }

type memBindingSessionRepo struct {
	sessions map[string]*domainauth.Session
}

func (r *memBindingSessionRepo) GetByID(_ context.Context, _, id string) (*domainauth.Session, error) {
	return r.sessions[id], nil
}
func (r *memBindingSessionRepo) Insert(context.Context, string, *domainauth.Session) error {
	return nil
}
func (r *memBindingSessionRepo) Delete(context.Context, string, string) error { return nil }
func (r *memBindingSessionRepo) DeleteByUser(context.Context, string, string) error {
	return nil
}
func (r *memBindingSessionRepo) DeleteOldestByUser(context.Context, string, string, int) error {
	return nil
}
func (r *memBindingSessionRepo) ListByUser(context.Context, string, string) ([]domainauth.Session, error) {
	return nil, nil
}

// stubCookieVerifier 把 cookie 值映射到（project, session），映射表由测试构造。
type stubCookieVerifier struct {
	entries map[string]cookieEntry
}

type cookieEntry struct {
	projectID string
	sessionID string
}

func (v *stubCookieVerifier) Verify(value string) (string, string, error) {
	e, ok := v.entries[value]
	if !ok {
		return "", "", status.Error(codes.Unauthenticated, "invalid session cookie")
	}
	return e.projectID, e.sessionID, nil
}

// ---- 装配 ----

func newOAuthBindingAccount() (*Account, *memOAuthStateStore, *memBindingSessionRepo, *stubCookieVerifier, *memIdentityRepo) {
	states := newMemOAuthStateStore()
	sessions := &memBindingSessionRepo{sessions: map[string]*domainauth.Session{}}
	verifier := &stubCookieVerifier{entries: map[string]cookieEntry{}}
	identities := &memIdentityRepo{byUID: map[string]*domainauth.Identity{}}
	a := NewAccount(
		&config.AppConfig{},
		stubOAuthProjectRepo{},
		stubOAuthProviderRepo{},
		stubSessionService{},
		nil, states, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		&memUserRepo{byID: map[string]*users.User{
			"user-victim":   {ID: "user-victim", Email: "victim@example.com", Status: users.StatusActive},
			"user-attacker": {ID: "user-attacker", Email: "attacker@example.com", Status: users.StatusActive},
		}},
		identities,
		sessions,
		stubOAuthFactory{},
		nil, nil,
		verifier,
	)
	return a, states, sessions, verifier, identities
}

func endUserBindingCtx(userID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		UserID:    userID,
		ProjectID: oauthBindingTestProject,
	})
}

func soleStateID(t *testing.T, states *memOAuthStateStore) string {
	t.Helper()
	require.Len(t, states.states, 1)
	for id := range states.states {
		return id
	}
	return ""
}

func startLinkSession(t *testing.T, a *Account, userID string) string {
	t.Helper()
	_, _, err := a.CreateOAuth2LinkSession(endUserBindingCtx(userID), CreateOAuth2LinkSessionCommand{
		ProjectID: oauthBindingTestProject,
		Provider:  "google",
		Success:   "https://app.example/callback",
		Failure:   "https://app.example/callback",
	})
	require.NoError(t, err)
	return soleStateID(t, a.oauthState.(*memOAuthStateStore))
}

// ---- 用例 ----

// login 成对：发起种 nonce（state 记录同值落库）→ 回调带配对 cookie → 成功。
func TestOAuth2Login_CallbackWithMatchingNonce(t *testing.T) {
	a, states, _, _, _ := newOAuthBindingAccount()

	_, nonce, err := a.CreateOAuth2Session(context.Background(), CreateOAuth2SessionCommand{
		ProjectID: oauthBindingTestProject,
		Provider:  "google",
		Success:   "https://app.example/callback",
		Failure:   "https://app.example/callback",
	})
	require.NoError(t, err)
	require.NotEmpty(t, nonce)
	stateID := soleStateID(t, states)
	require.Equal(t, nonce, states.states[stateID].Nonce, "state 记录必须携带发起时的 nonce")

	result, err := a.HandleOAuth2Callback(context.Background(), "google", "code-1", stateID,
		map[string]string{}, map[string]string{oauthBindingTestProject: nonce})
	require.NoError(t, err)
	require.NotNil(t, result.User)
}

// login 不配对：nonce 缺失/不匹配 → Unauthenticated（fail-closed）。
func TestOAuth2Login_CallbackWithMismatchedNonce(t *testing.T) {
	a, _, _, _, _ := newOAuthBindingAccount()

	startLogin := func() string {
		_, _, err := a.CreateOAuth2Session(context.Background(), CreateOAuth2SessionCommand{
			ProjectID: oauthBindingTestProject,
			Provider:  "google",
			Success:   "https://app.example/callback",
			Failure:   "https://app.example/callback",
		})
		require.NoError(t, err)
		return soleStateID(t, a.oauthState.(*memOAuthStateStore))
	}

	// 错误 nonce。
	stateID := startLogin()
	_, err := a.HandleOAuth2Callback(context.Background(), "google", "code-x", stateID,
		nil, map[string]string{oauthBindingTestProject: "wrong-nonce"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// 缺失 nonce（空 map 也 fail-closed）。
	stateID = startLogin()
	_, err = a.HandleOAuth2Callback(context.Background(), "google", "code-x", stateID,
		nil, map[string]string{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// link 本人：回调携带 link 目标用户的项目会话 cookie → 允许并完成 link。
func TestOAuth2Link_CallbackAsOwner(t *testing.T) {
	a, _, sessions, verifier, _ := newOAuthBindingAccount()

	stateID := startLinkSession(t, a, "user-victim")

	sessions.sessions["sess-victim"] = &domainauth.Session{
		ID: "sess-victim", UserID: "user-victim", ExpireAt: time.Now().Add(time.Hour),
	}
	verifier.entries["cookie-victim"] = cookieEntry{oauthBindingTestProject, "sess-victim"}

	result, err := a.HandleOAuth2Callback(context.Background(), "google", "code-v", stateID,
		map[string]string{oauthBindingTestProject: "cookie-victim"}, nil)
	require.NoError(t, err)
	require.NotNil(t, result.User)
	require.Equal(t, "user-victim", result.User.ID)
}

// link 冒名：他人会话 cookie / 无 cookie / token 面他人消费 → 均
// PermissionDenied，且不落任何 identity。
func TestOAuth2Link_CallbackAsImpostor(t *testing.T) {
	a, _, sessions, verifier, identities := newOAuthBindingAccount()

	// ① 冒名者本人的会话 cookie：非 link 目标 → 拒绝。
	stateID := startLinkSession(t, a, "user-victim")
	sessions.sessions["sess-attacker"] = &domainauth.Session{
		ID: "sess-attacker", UserID: "user-attacker", ExpireAt: time.Now().Add(time.Hour),
	}
	verifier.entries["cookie-attacker"] = cookieEntry{oauthBindingTestProject, "sess-attacker"}

	result, err := a.HandleOAuth2Callback(context.Background(), "google", "code-a", stateID,
		map[string]string{oauthBindingTestProject: "cookie-attacker"}, nil)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Nil(t, result.User)
	require.Empty(t, identities.byUID, "冒名 link 不得落 identity")

	// ② 无会话 cookie 同样拒绝。
	stateID = startLinkSession(t, a, "user-victim")
	result, err = a.HandleOAuth2Callback(context.Background(), "google", "code-a2", stateID,
		map[string]string{}, nil)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// ③ token 面：link-token 方法以他人身份消费 → PermissionDenied。
	stateID = startLinkSession(t, a, "user-victim")
	_, err = a.CreateOAuth2LinkTokenSession(endUserBindingCtx("user-attacker"), CreateOAuth2LinkTokenSessionCommand{
		ProjectID: oauthBindingTestProject,
		Provider:  "google",
		Code:      "code-t",
		State:     stateID,
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Empty(t, identities.byUID)
}
