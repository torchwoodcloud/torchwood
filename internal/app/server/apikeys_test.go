package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeAPIKeyRepository struct{}

// testScopeVocabulary 构造覆盖常用资源（读+写双向）的词表（真实词表由
// PolicySet 派生；本测试面需覆盖历史合法 scope 形态全集）。
func testScopeVocabulary() *domainauth.ScopeVocabulary {
	resources := []domainauth.ScopeResource{
		domainauth.ScopeUsers, domainauth.ScopeDatabases, domainauth.ScopeStorage,
		domainauth.ScopeGroups, domainauth.ScopeProjects, domainauth.ScopeOAuthProviders,
		domainauth.ScopeFunctions, domainauth.ScopePayments, domainauth.ScopeAssets,
		domainauth.ScopeSubscriptions, domainauth.ScopeBilling, domainauth.ScopeOutbox,
	}
	pols := make([]domainauth.MethodPolicy, 0, len(resources)*2)
	for i, res := range resources {
		for j, op := range []domainauth.ScopeOp{domainauth.ScopeRead, domainauth.ScopeWrite} {
			pols = append(pols, domainauth.MethodPolicy{
				Method: fmt.Sprintf("/t/r%d/%d", i, j), Service: "/t",
				Access: domainauth.AccessServer,
				Scope:  &domainauth.ScopeRule{Resource: res, Op: op},
			})
		}
	}
	set, err := domainauth.NewPolicySet(pols)
	if err != nil {
		panic(err)
	}
	return domainauth.VocabularyFromPolicies(set)
}

func (f *fakeAPIKeyRepository) CreateAPIKey(ctx context.Context, key *projects.APIKey) error {
	return nil
}
func (f *fakeAPIKeyRepository) GetAPIKey(ctx context.Context, projectID, id string) (*projects.APIKey, error) {
	return nil, nil
}
func (f *fakeAPIKeyRepository) GetAPIKeyBySecretHash(ctx context.Context, hash string) (*projects.APIKey, error) {
	return nil, nil
}
func (f *fakeAPIKeyRepository) ListAPIKeys(ctx context.Context, projectID string) ([]projects.APIKey, error) {
	return nil, nil
}
func (f *fakeAPIKeyRepository) DeleteAPIKey(ctx context.Context, projectID, id string) error {
	return nil
}

// TestAPIKeys_Create_SecretFormat：明文 secret 带 sk- 前缀（idgen.APIKeySecret
// 单一事实源），SecretHash 为该明文的 SHA-256——校验侧按哈希反查，前缀纯标识。
func TestAPIKeys_Create_SecretFormat(t *testing.T) {
	uc := NewAPIKeys(&fakeAPIKeyRepository{}, testScopeVocabulary())

	key, secret, err := uc.CreateInternal(context.Background(), CreateAPIKeyCommand{Name: "k", Scopes: []string{"*"}})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(secret, idgen.APIKeySecretPrefix), "secret 应带 %q 前缀: %s", idgen.APIKeySecretPrefix, secret)
	hash := sha256.Sum256([]byte(secret))
	require.Equal(t, hex.EncodeToString(hash[:]), key.SecretHash)
}

// TestAPIKeys_Create_ScopeValidation (B2): Create 时校验 scope 格式
// ∈ {*, all, 裸资源名, <resource>.read, <resource>.write}，上限 32 项/64 字符。
func TestAPIKeys_Create_ScopeValidation(t *testing.T) {
	uc := NewAPIKeys(&fakeAPIKeyRepository{}, testScopeVocabulary())
	ctx := platformAdminCtx(context.Background())

	for _, scopes := range [][]string{
		{"*"},
		{"all"},
		{"databases"},
		{"databases.read", "databases.write"},
		{"users.read", "storage.write", "oauthproviders", "groups"},
	} {
		_, _, err := uc.Create(ctx, CreateAPIKeyCommand{Name: "k", Scopes: scopes})
		require.NoError(t, err, "scopes %v should be accepted", scopes)
	}

	for _, scopes := range [][]string{
		{"foo"},
		{"health"},
		{"health.read"},
		{"databases.delete"},
		{"databases.read.extra"},
		{"any"},
		{""},
	} {
		_, _, err := uc.Create(ctx, CreateAPIKeyCommand{Name: "k", Scopes: scopes})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "scopes %v should be rejected", scopes)
	}

	// 超过 32 项拒绝。
	overCount := make([]string, 33)
	for i := range overCount {
		overCount[i] = "databases.read"
	}
	_, _, err := uc.Create(ctx, CreateAPIKeyCommand{Name: "k", Scopes: overCount})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 单项超过 64 字符拒绝。
	scope := "databases.read"
	for len(scope) <= 64 {
		scope += "x"
	}
	_, _, err = uc.Create(ctx, CreateAPIKeyCommand{Name: "k", Scopes: []string{scope}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 空 scope 拒绝。
	_, _, err = uc.Create(ctx, CreateAPIKeyCommand{Name: "k"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestAPIKeys_Create_RequiresPlatformAdmin（F2-2 纵深防御）：受限 admin
// （viewer/member）与 API key 主体调用 Create 必须 PermissionDenied。
func TestAPIKeys_Create_RequiresPlatformAdmin(t *testing.T) {
	uc := NewAPIKeys(&fakeAPIKeyRepository{}, testScopeVocabulary())

	for _, principal := range []*shared.Principal{
		{ActorID: "admin-2", ActorKind: shared.ActorKindAdmin, Roles: []string{"viewer"}},
		{ActorID: "admin-3", ActorKind: shared.ActorKindAdmin, Roles: []string{"member"}},
		{ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"}, Permissions: []string{"*"}},
	} {
		ctx := contexts.WithPrincipal(context.Background(), principal)
		_, _, err := uc.Create(ctx, CreateAPIKeyCommand{Name: "k", Scopes: []string{"*"}})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "principal %+v should be denied", principal)
	}

	// 平台 admin 放行。
	_, _, err := uc.Create(platformAdminCtx(context.Background()), CreateAPIKeyCommand{Name: "k", Scopes: []string{"*"}})
	require.NoError(t, err)
}

// Round3 H1-3：APIKeys.Delete 与 Create 对齐的平台级守卫——端用户/API key/
// 受限 admin（viewer/member）一律 PermissionDenied，匿名 Unauthenticated；
// 平台 admin 通过守卫后进入业务路径（key 不存在 → NotFound）。
func TestAPIKeys_Delete_RequiresPlatformAdmin(t *testing.T) {
	uc := NewAPIKeys(&fakeAPIKeyRepository{}, testScopeVocabulary())

	for _, principal := range []*shared.Principal{
		{ActorID: "user-1", ActorKind: shared.ActorKindEndUser, UserID: "user-1"},
		{ActorID: "admin-2", ActorKind: shared.ActorKindAdmin, Roles: []string{"viewer"}},
		{ActorID: "admin-3", ActorKind: shared.ActorKindAdmin, Roles: []string{"member"}},
		{ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"}, Permissions: []string{"*"}},
	} {
		ctx := contexts.WithPrincipal(context.Background(), principal)
		err := uc.Delete(ctx, "p1", "key-1")
		require.Equal(t, codes.PermissionDenied, status.Code(err), "principal %+v Delete 应被拒", principal)
	}

	err := uc.Delete(context.Background(), "p1", "key-1")
	require.Equal(t, codes.Unauthenticated, status.Code(err), "匿名 Delete 应被拒")

	err = uc.Delete(platformAdminCtx(context.Background()), "p1", "key-1")
	require.Equal(t, codes.NotFound, status.Code(err), "平台 admin 应通过守卫，随后因 key 不存在返回 NotFound")
}

// TestAPIKeys_CrossProjectGetDeleteNotFound：仓储 project_id 谓词生效后，
// 跨项目 Get/Delete 一律 404，不得靠 Go 侧比对。
func TestAPIKeys_CrossProjectGetDeleteNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	p1, _, c1 := testutil.CreateTestProject(ctx, db)
	defer c1()
	p2, _, c2 := testutil.CreateTestProject(ctx, db)
	defer c2()

	uc := NewAPIKeys(bunrepo.NewAPIKeyRepository(db), testScopeVocabulary())
	key, _, err := uc.CreateInternal(ctx, CreateAPIKeyCommand{
		ProjectID: p1,
		Name:      "k",
		Scopes:    []string{"*"},
	})
	require.NoError(t, err)

	_, err = uc.Get(ctx, p2, key.ID)
	require.Equal(t, codes.NotFound, status.Code(err), "跨项目 GetAPIKey → 404")

	err = uc.Delete(platformAdminCtx(ctx), p2, key.ID)
	require.Equal(t, codes.NotFound, status.Code(err), "跨项目 DeleteAPIKey → 404")

	got, err := uc.Get(ctx, p1, key.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, key.ID, got.ID)
}

// TestAPIKeys_EnsureScopesWithinCaller（G2-5/R06-P3）：cmd.Scopes 必须 ⊆
// 调用者 principal.Permissions，超出返回 PermissionDenied；平台 admin 放行。
func TestAPIKeys_EnsureScopesWithinCaller(t *testing.T) {
	// 平台 admin（会话权限不按 scope 建模）恒放行。
	require.NoError(t, ensureScopesWithinCaller(platformAdminCtx(context.Background()), []string{"*", "users.write"}))

	// 受限主体：scope 全部包含于自身 Permissions 时放行。
	restrictedCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:     "admin-2",
		ActorKind:   shared.ActorKindAdmin,
		Roles:       []string{"member"},
		Permissions: []string{"users.read", "storage.write"},
	})
	require.NoError(t, ensureScopesWithinCaller(restrictedCtx, []string{"users.read"}))

	// 超范围 scope → PermissionDenied（含通配符与部分超范围）。
	for _, scopes := range [][]string{
		{"users.write"},
		{"users.read", "storage.write", "databases"},
		{"*"},
	} {
		err := ensureScopesWithinCaller(restrictedCtx, scopes)
		require.Equal(t, codes.PermissionDenied, status.Code(err), "scopes %v 超出调用者权限必须拒绝", scopes)
	}

	// 匿名 → Unauthenticated。
	err := ensureScopesWithinCaller(context.Background(), []string{"users.read"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
