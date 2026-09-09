package serverhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// G4-1：多凭证拒绝——与 gRPC extractCredential 语义一致，
// X-Api-Key / Authorization / session cookie 任意两种并存 → 401 Unauthenticated。
func TestHTTPAuth_MultipleCredentialsRejected(t *testing.T) {
	t.Parallel()

	a := newHTTPAuth(newFunctionsValidator(&functionsDocDB{}), testPolicies())

	cases := []struct {
		name  string
		apply func(r *http.Request)
	}{
		{
			name: "x-api-key + authorization",
			apply: func(r *http.Request) {
				r.Header.Set("X-Api-Key", "key")
				r.Header.Set("Authorization", "Bearer token")
			},
		},
		{
			name: "x-api-key + session cookie",
			apply: func(r *http.Request) {
				r.Header.Set("X-Api-Key", "key")
				r.AddCookie(&http.Cookie{Name: "TORCHWOOD_session_proj-1", Value: "token"}) // #nosec G124 -- 客户端测试请求无需安全属性
			},
		},
		{
			name: "authorization + session cookie",
			apply: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer token")
				r.AddCookie(&http.Cookie{Name: "TORCHWOOD_session_proj-1", Value: "token"}) // #nosec G124 -- 客户端测试请求无需安全属性
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			tc.apply(r)
			_, err := a.authenticate(r)
			require.Equal(t, codes.Unauthenticated, status.Code(err))
			require.Contains(t, err.Error(), "multiple credentials provided")
		})
	}
}

// G11-2：同一凭证 key 多值（双 X-Api-Key / 双 Authorization / 双 session
// cookie）→ 401 multiple credentials provided，与 gRPC credentialMetadataValue 对齐。
func TestHTTPAuth_SameKeyMultipleValuesRejected(t *testing.T) {
	t.Parallel()

	a := newHTTPAuth(newFunctionsValidator(&functionsDocDB{}), testPolicies())

	cases := []struct {
		name  string
		apply func(r *http.Request)
	}{
		{
			name: "duplicate x-api-key headers",
			apply: func(r *http.Request) {
				r.Header.Add("X-Api-Key", "key-1")
				r.Header.Add("X-Api-Key", "key-2")
			},
		},
		{
			name: "duplicate authorization headers",
			apply: func(r *http.Request) {
				r.Header.Add("Authorization", "Bearer token-1")
				r.Header.Add("Authorization", "Bearer token-2")
			},
		},
		{
			name: "duplicate session cookies",
			apply: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: "TORCHWOOD_session_proj-1", Value: "tok-1"}) // #nosec G124 -- 客户端测试请求无需安全属性
				r.AddCookie(&http.Cookie{Name: "TORCHWOOD_session_proj-1", Value: "tok-2"}) // #nosec G124 -- 客户端测试请求无需安全属性
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			tc.apply(r)
			_, err := a.authenticate(r)
			require.Equal(t, codes.Unauthenticated, status.Code(err))
			require.Contains(t, err.Error(), "multiple credentials provided")
		})
	}
}

// G4-1：单凭证请求不受影响（API key 正常认证；无凭证 401）。
func TestHTTPAuth_SingleCredentialAccepted(t *testing.T) {
	t.Parallel()

	repo := &functionsAPIKeyRepo{keys: map[string]*projects.APIKey{
		functionsHashSecret("auth-key-ok"): {
			ID:        "key-1",
			ProjectID: "proj-1",
			Scopes:    []string{"storage.read"},
			Enabled:   true,
		},
	}}
	validator := auth.NewValidator(functionsTestConfig(), repo, nil, &functionsAdminRepo{}, &functionsAdminProjectRepo{}, nil, nil, nil, nil)
	a := newHTTPAuth(validator, testPolicies())

	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("X-Api-Key", "auth-key-ok")
	p, err := a.authenticate(r)
	require.NoError(t, err)
	require.Equal(t, shared.CredentialTypeAPIKey, p.CredentialType)
	require.Equal(t, "proj-1", p.ProjectID)

	// 非 session 前缀的普通 cookie 不算凭证，也不会误伤无凭证路径。
	rNoCred := httptest.NewRequest(http.MethodGet, "/x", nil)
	rNoCred.AddCookie(&http.Cookie{Name: "tracking", Value: "abc"}) // #nosec G124 -- 客户端测试请求无需安全属性
	_, err = a.authenticate(rNoCred)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// G4-1 handler 级：FileHandler 下载路径多凭证 → HTTP 401。
func TestFileHandler_MultipleCredentialsRejected(t *testing.T) {
	t.Parallel()

	h, err := NewFileHandler(functionsTestConfig(), newFunctionsValidator(&functionsDocDB{}), nil, nil, nil, testPolicies())
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, "/v1/storage/buckets/b-1/files/f-1/download", nil)
	r.Header.Set("X-Api-Key", "key")
	r.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.download(rec, r, map[string]string{"bucketId": "b-1", "fileId": "f-1"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// G4-1 handler 级：FunctionsHandler 上传路径多凭证 → HTTP 401。
func TestFunctionsHandler_MultipleCredentialsRejected(t *testing.T) {
	t.Parallel()

	h := newFunctionsHandler(t, newFunctionsValidator(&functionsDocDB{}))

	r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", nil)
	r.Header.Set("X-Api-Key", "key")
	r.AddCookie(&http.Cookie{Name: "TORCHWOOD_session_proj-1", Value: "token"}) // #nosec G124 -- 客户端测试请求无需安全属性
	rec := httptest.NewRecorder()
	h.upload(rec, r, map[string]string{"functionId": "fn-1"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// testPolicies 构造最小策略注册表（真实策略由 runtime.PolicySet 注入）。
func testPolicies() *domainauth.PolicySet {
	set, err := domainauth.NewPolicySet([]domainauth.MethodPolicy{
		{Method: domainauth.StorageServiceCreateFile, Service: "/torchwood.server.v1.StorageService", Access: domainauth.AccessServer,
			AdminRoles: []domainauth.AdminRole{domainauth.AdminRoleMember, domainauth.AdminRoleAdmin, domainauth.AdminRoleOwner},
			Scope:      &domainauth.ScopeRule{Resource: domainauth.ScopeStorage, Op: domainauth.ScopeWrite}},
		{Method: domainauth.StorageServiceGetFile, Service: "/torchwood.server.v1.StorageService", Access: domainauth.AccessServer,
			Scope: &domainauth.ScopeRule{Resource: domainauth.ScopeStorage, Op: domainauth.ScopeRead}},
		{Method: FunctionsServiceCreateDeployment, Service: "/torchwood.server.v1.FunctionsService", Access: domainauth.AccessServer,
			AdminRoles: []domainauth.AdminRole{domainauth.AdminRoleAdmin, domainauth.AdminRoleOwner},
			Scope:      &domainauth.ScopeRule{Resource: domainauth.ScopeFunctions, Op: domainauth.ScopeWrite}},
	})
	if err != nil {
		panic(err)
	}
	return set
}
