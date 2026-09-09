package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testJWTSecret = "validator-test-secret"

func testValidatorConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: testJWTSecret},
		},
	}
}

type memAdminRevokeStore struct {
	revoked map[string]time.Time
}

func newMemAdminRevokeStore() *memAdminRevokeStore {
	return &memAdminRevokeStore{revoked: map[string]time.Time{}}
}

// memOneTimeTokenStore 是内存版 OneTimeTokenStore 桩：Register 记录，
// Consume 原子取删；记录消费次数以便断言普通 token 不触碰消费路径。
type memOneTimeTokenStore struct {
	records  map[string]string
	consumed int
}

func newMemOneTimeTokenStore() *memOneTimeTokenStore {
	return &memOneTimeTokenStore{records: map[string]string{}}
}

func (s *memOneTimeTokenStore) Register(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	if _, exists := s.records[key]; exists {
		return false, nil
	}
	s.records[key] = value
	return true, nil
}

func (s *memOneTimeTokenStore) Consume(_ context.Context, key string) (string, error) {
	value, ok := s.records[key]
	if !ok {
		return "", nil
	}
	delete(s.records, key)
	s.consumed++
	return value, nil
}

var _ domainauth.OneTimeTokenStore = (*memOneTimeTokenStore)(nil)

func (s *memAdminRevokeStore) RevokeBefore(_ context.Context, adminID string, revokedAt time.Time, _ time.Duration) error {
	if existing, ok := s.revoked[adminID]; !ok || revokedAt.After(existing) {
		s.revoked[adminID] = revokedAt
	}
	return nil
}

func (s *memAdminRevokeStore) RevokedBefore(_ context.Context, adminID string) (time.Time, error) {
	return s.revoked[adminID], nil
}

var _ domainauth.AdminTokenRevokeStore = (*memAdminRevokeStore)(nil)

type stubAPIKeyRepo struct {
	keys map[string]*projects.APIKey
}

func (r *stubAPIKeyRepo) CreateAPIKey(context.Context, *projects.APIKey) error { return nil }
func (r *stubAPIKeyRepo) GetAPIKey(context.Context, string, string) (*projects.APIKey, error) {
	return nil, nil
}
func (r *stubAPIKeyRepo) GetAPIKeyBySecretHash(_ context.Context, hash string) (*projects.APIKey, error) {
	return r.keys[hash], nil
}
func (r *stubAPIKeyRepo) ListAPIKeys(context.Context, string) ([]projects.APIKey, error) {
	return nil, nil
}
func (r *stubAPIKeyRepo) UpdateAPIKey(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *stubAPIKeyRepo) DeleteAPIKey(context.Context, string, string) error { return nil }

type stubAdminRepo struct {
	admins map[string]*projects.Admin
}

func (r *stubAdminRepo) GetAdmin(_ context.Context, id string) (*projects.Admin, error) {
	return r.admins[id], nil
}
func (r *stubAdminRepo) GetAdminByEmail(context.Context, string) (*projects.Admin, error) {
	return nil, nil
}
func (r *stubAdminRepo) ListAdmins(context.Context) ([]projects.Admin, error) {
	return nil, nil
}
func (r *stubAdminRepo) CreateAdmin(context.Context, *projects.Admin) error {
	return nil
}
func (r *stubAdminRepo) UpdateAdmin(context.Context, *projects.Admin) error {
	return nil
}
func (r *stubAdminRepo) RevokeCredentials(_ context.Context, adminID string, revokedAt time.Time) error {
	if a, ok := r.admins[adminID]; ok {
		a.RevokedAt = revokedAt
	}
	return nil
}
func (r *stubAdminRepo) DeleteAdmin(context.Context, string) error {
	return nil
}
func (r *stubAdminRepo) CountAdminsByRole(context.Context, string) (int64, error) {
	return 1, nil
}

func (r *stubAdminRepo) WithBootstrapLock(_ context.Context, _ int64, fn func(ctx context.Context) error) error {
	return fn(context.Background())
}

type stubAdminProjectRepo struct {
	access map[string]map[string]struct{}
}

func (r *stubAdminProjectRepo) HasProjectAccess(_ context.Context, adminID, projectID string) (bool, error) {
	if projects, ok := r.access[adminID]; ok {
		_, ok := projects[projectID]
		return ok, nil
	}
	return false, nil
}
func (r *stubAdminProjectRepo) GrantProjectAccess(context.Context, string, string) error { return nil }
func (r *stubAdminProjectRepo) ListProjectIDs(context.Context, string) ([]string, error) {
	return nil, nil
}

type stubProjectRepo struct {
	projects map[string]*projects.Project
}

func (r *stubProjectRepo) CreateProject(context.Context, *projects.Project) error { return nil }
func (r *stubProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	return r.projects[id], nil
}
func (r *stubProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (r *stubProjectRepo) ListProjects(context.Context) ([]projects.Project, error)    { return nil, nil }
func (r *stubProjectRepo) UpdateProject(context.Context, *projects.Project) error      { return nil }
func (r *stubProjectRepo) DeleteProject(context.Context, string) error                 { return nil }
func (r *stubProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }

type stubDocDB struct {
	users          map[string]map[string]map[string]any
	sessions       map[string]map[string]map[string]any
	failBulkDelete bool
}

func (d *stubDocDB) GetDocument(_ context.Context, projectID, _, collectionID, docID string, _ databases.Principal) (*databases.Document, error) {
	switch collectionID {
	case "users":
		if data, ok := d.users[projectID][docID]; ok {
			return &databases.Document{ID: docID, Data: data}, nil
		}
	case "sessions":
		if data, ok := d.sessions[projectID][docID]; ok {
			return &databases.Document{ID: docID, Data: data}, nil
		}
	}
	return nil, nil
}

// CreateDocument 只支持 sessions 集合（会话测试桩）。
func (d *stubDocDB) CreateDocument(_ context.Context, projectID, _, collectionID string, doc databases.Document, _ []databases.Permission, _ databases.Principal) (databases.Document, error) {
	if collectionID == "sessions" {
		if d.sessions == nil {
			d.sessions = map[string]map[string]map[string]any{}
		}
		if d.sessions[projectID] == nil {
			d.sessions[projectID] = map[string]map[string]any{}
		}
		d.sessions[projectID][doc.ID] = doc.Data
		return doc, nil
	}
	return doc, nil
}

// ListDocuments 只支持 sessions 集合：equal("user_id") 过滤 +
// orderAsc("expire_at") 升序（RFC3339Nano 字符串序 == 时间序），
// 不做分页（测试数据均为单页）。
func (d *stubDocDB) ListDocuments(_ context.Context, projectID, _, collectionID string, q databases.Query, _ databases.Principal) (*databases.DocumentList, error) {
	if collectionID != "sessions" {
		return &databases.DocumentList{}, nil
	}
	parsed := q.AST
	if parsed == nil {
		var err error
		parsed, err = query.ParseMany(q.Queries)
		if err != nil {
			return nil, err
		}
	}
	var docs []databases.Document
	for id, data := range d.sessions[projectID] {
		match := true
		parsed.WalkLeaves(func(f query.Filter) {
			if f.Op == query.OpEqual && len(f.Values) > 0 && fmt.Sprint(data[f.Attribute]) != f.Values[0] {
				match = false
			}
		})
		if match {
			docs = append(docs, databases.Document{ID: id, Data: data})
		}
	}
	sort.SliceStable(docs, func(i, j int) bool {
		return fmt.Sprint(docs[i].Data["expire_at"]) < fmt.Sprint(docs[j].Data["expire_at"])
	})
	if parsed.Limit > 0 && len(docs) > parsed.Limit {
		docs = docs[:parsed.Limit]
	}
	return &databases.DocumentList{Documents: docs}, nil
}

// BulkDeleteDocuments 支持 sessions 集合；failBulkDelete 置位时注入故障。
func (d *stubDocDB) BulkDeleteDocuments(_ context.Context, projectID, _, collectionID string, documentIDs []string, _ databases.Principal) (int64, error) {
	if d.failBulkDelete {
		return 0, fmt.Errorf("injected bulk delete failure")
	}
	if collectionID != "sessions" {
		return 0, nil
	}
	var deleted int64
	for _, id := range documentIDs {
		if _, ok := d.sessions[projectID][id]; ok {
			delete(d.sessions[projectID], id)
			deleted++
		}
	}
	return deleted, nil
}

func (d *stubDocDB) CreateDatabase(context.Context, string, string, string) error { return nil }
func (d *stubDocDB) GetDatabase(context.Context, string, string) (*databases.Database, error) {
	return nil, nil
}
func (d *stubDocDB) ListDatabases(context.Context, string) ([]databases.Database, error) {
	return nil, nil
}
func (d *stubDocDB) DeleteDatabase(context.Context, string, string) error { return nil }
func (d *stubDocDB) CreateCollection(context.Context, string, string, string, string, []databases.Attribute, []databases.Index, []databases.Permission, bool) error {
	return nil
}
func (d *stubDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return nil, nil
}
func (d *stubDocDB) ListCollections(context.Context, string, string, databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (d *stubDocDB) DeleteCollection(context.Context, string, string, string) error { return nil }
func (d *stubDocDB) UpdateCollection(context.Context, string, string, string, databases.CollectionPatch) error {
	return nil
}
func (d *stubDocDB) CreateAttribute(context.Context, string, string, string, databases.Attribute) error {
	return nil
}
func (d *stubDocDB) DeleteAttribute(context.Context, string, string, string, string) error {
	return nil
}
func (d *stubDocDB) CreateIndex(context.Context, string, string, string, databases.Index) error {
	return nil
}
func (d *stubDocDB) DeleteIndex(context.Context, string, string, string, string) error { return nil }
func (d *stubDocDB) UpdateDocument(context.Context, string, string, string, databases.DocumentUpdate, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *stubDocDB) UpsertDocument(context.Context, string, string, string, databases.Document, []string, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *stubDocDB) DeleteDocument(context.Context, string, string, string, string, databases.DeleteOptions, databases.Principal) error {
	return nil
}
func (d *stubDocDB) CountDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *stubDocDB) AggregateDocuments(context.Context, string, string, string, databases.Query, []databases.AggregateSpec, string, databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (d *stubDocDB) BulkUpdateDocuments(context.Context, string, string, string, []string, map[string]any, []databases.Permission, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *stubDocDB) EnsureCatalog(context.Context, string) error { return nil }

func (*stubDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (*stubDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	panic("ListChanges: not implemented in test fake")
}

var _ databases.DocumentDB = (*stubDocDB)(nil)

func hashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func signToken(t *testing.T, claims jwtparser.Claims) string {
	t.Helper()
	purpose := jwtparser.PurposeEndUserJWT
	if claims.ActorKind == "admin" {
		purpose = jwtparser.PurposeAdminJWT
	}
	token, err := jwtparser.Generate(jwtparser.DeriveKey(testJWTSecret, purpose), claims)
	require.NoError(t, err)
	return token
}

func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, want, st.Code())
}

func TestValidator_ValidateAPIKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secret := "torchwood-test-api-key"
	key := &projects.APIKey{
		ID:        "key-1",
		ProjectID: "proj-1",
		Scopes:    []string{"storage", "users"},
		Enabled:   true,
	}
	repo := &stubAPIKeyRepo{keys: map[string]*projects.APIKey{hashSecret(secret): key}}
	v := auth.NewValidator(testValidatorConfig(), repo, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, nil, nil)

	p, err := v.ValidateCredential(ctx, secret, shared.CredentialTypeAPIKey)
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindService, p.ActorKind)
	require.Equal(t, "proj-1", p.ProjectID)
	require.Equal(t, []string{"storage", "users"}, p.Permissions)
	// B14 per-key 角色：keys（scope/API 面）与 key:<id>（数据隔离身份）并存。
	require.Equal(t, []string{"keys", "key:key-1"}, p.Roles)

	key.Enabled = false
	_, err = v.ValidateCredential(ctx, secret, shared.CredentialTypeAPIKey)
	requireCode(t, err, codes.Unauthenticated)
}

func TestValidator_ValidateAPIKey_InactiveProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secret := "torchwood-test-api-key-2"
	key := &projects.APIKey{
		ID:        "key-2",
		ProjectID: "proj-2",
		Scopes:    []string{"storage"},
		Enabled:   true,
	}
	repo := &stubAPIKeyRepo{keys: map[string]*projects.APIKey{hashSecret(secret): key}}
	projRepo := &stubProjectRepo{projects: map[string]*projects.Project{
		"proj-2": {ID: "proj-2", Status: "suspended"},
	}}
	v := auth.NewValidator(testValidatorConfig(), repo, projRepo, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, nil, nil)
	_, err := v.ValidateCredential(ctx, secret, shared.CredentialTypeAPIKey)
	requireCode(t, err, codes.Unauthenticated)

	// active 项目放行
	projRepo.projects["proj-2"].Status = "active"
	p, err := v.ValidateCredential(ctx, secret, shared.CredentialTypeAPIKey)
	require.NoError(t, err)
	require.Equal(t, "proj-2", p.ProjectID)

	// projectRepo 为 nil 时不检查（兼容旧测试）
	v2 := auth.NewValidator(testValidatorConfig(), repo, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, nil, nil)
	p, err = v2.ValidateCredential(ctx, secret, shared.CredentialTypeAPIKey)
	require.NoError(t, err)
	require.Equal(t, "proj-2", p.ProjectID)
}

func TestValidator_ValidateAdminJWT(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admin := &projects.Admin{
		ID:    "admin-1",
		Email: "admin@torchwood.local",
		Role:  "member",
	}
	admins := &stubAdminRepo{admins: map[string]*projects.Admin{admin.ID: admin}}
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, admins, &stubAdminProjectRepo{}, nil, nil, nil, nil)

	token := signToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		Username:  admin.Email,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	p, err := v.ValidateToken(ctx, token)
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindAdmin, p.ActorKind)
	require.Equal(t, admin.ID, p.AdminID)
	require.Empty(t, p.UserID)
	require.True(t, p.IsAuthenticated())
	require.False(t, p.IsPlatformAdmin)
	require.False(t, p.IsSystem())
}

func TestValidator_ValidateAdminJWT_RejectsRefreshToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admin := &projects.Admin{
		ID:    "admin-1",
		Email: "admin@torchwood.local",
		Role:  "owner",
	}
	admins := &stubAdminRepo{admins: map[string]*projects.Admin{admin.ID: admin}}
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, admins, &stubAdminProjectRepo{}, nil, nil, nil, nil)

	token := signToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		Username:  admin.Email,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeRefresh,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour).Unix(),
	})
	_, err := v.ValidateToken(ctx, token)
	requireCode(t, err, codes.Unauthenticated)
}

func TestValidator_ValidateAdminJWT_Revoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	issuedAt := time.Now().Add(-time.Hour).Unix()
	admin := &projects.Admin{ID: "admin-1", Email: "admin@torchwood.local", Role: "owner"}
	revokeStore := newMemAdminRevokeStore()
	require.NoError(t, revokeStore.RevokeBefore(ctx, admin.ID, time.Now(), time.Hour))

	v := auth.NewValidator(
		testValidatorConfig(),
		&stubAPIKeyRepo{},
		nil,
		&stubAdminRepo{admins: map[string]*projects.Admin{admin.ID: admin}},
		&stubAdminProjectRepo{},
		revokeStore,
		nil,
		nil,
		nil,
	)
	token := signToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  issuedAt,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	_, err := v.ValidateToken(ctx, token)
	requireCode(t, err, codes.Unauthenticated)
}

// M5 C1：撤销事实源收敛到 DB（revoked_at 随 admins 行读出，零额外查询）。
// iat <= revoked_at 即拒（与 Redis 判定同构）；iat > revoked_at 与未撤销
// 不受影响；DB 与 Redis 并存时取两者 max。
func TestValidator_ValidateAdminJWT_RevokedInDB(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// 撤销时刻取过去时刻：fresh 分支的 iat 需落在过去（WithIssuedAt 拒未来 iat）。
	revokedAt := time.Now().Add(-time.Hour)
	admin := &projects.Admin{ID: "admin-1", Email: "admin@torchwood.local", Role: "owner", RevokedAt: revokedAt}
	admins := &stubAdminRepo{admins: map[string]*projects.Admin{admin.ID: admin}}
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, admins, &stubAdminProjectRepo{}, nil, nil, nil, nil)

	staleToken := signToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  revokedAt.Add(-time.Hour).Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	_, err := v.ValidateToken(ctx, staleToken)
	requireCode(t, err, codes.Unauthenticated)

	// 撤销时刻整秒边界同样拒绝（与 Redis 快路径 iat <= 判定一致）。
	boundaryToken := signToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  revokedAt.Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	_, err = v.ValidateToken(ctx, boundaryToken)
	requireCode(t, err, codes.Unauthenticated)

	// 撤销之后签发的新 token 不受影响。
	freshToken := signToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  revokedAt.Add(time.Minute).Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	p, err := v.ValidateToken(ctx, freshToken)
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindAdmin, p.ActorKind)
}

func TestValidator_ValidateAdminJWT_RevokeTakesMaxOfDBAndRedis(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	redisRevoked := time.Now().Add(-2 * time.Hour)
	dbRevoked := time.Now().Add(-time.Hour)
	admin := &projects.Admin{ID: "admin-1", Email: "admin@torchwood.local", Role: "owner", RevokedAt: dbRevoked}
	revokeStore := newMemAdminRevokeStore()
	require.NoError(t, revokeStore.RevokeBefore(ctx, admin.ID, redisRevoked, time.Hour))

	v := auth.NewValidator(
		testValidatorConfig(),
		&stubAPIKeyRepo{},
		nil,
		&stubAdminRepo{admins: map[string]*projects.Admin{admin.ID: admin}},
		&stubAdminProjectRepo{},
		revokeStore,
		nil,
		nil,
		nil,
	)
	// iat 介于 Redis 与 DB 撤销时刻之间：只看 Redis 会放行，取 max 后必须拒绝。
	token := signToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  redisRevoked.Add(time.Minute).Unix(),
		ExpiresAt: dbRevoked.Add(time.Hour).Unix(),
	})
	_, err := v.ValidateToken(ctx, token)
	requireCode(t, err, codes.Unauthenticated)
}

func TestValidator_ValidateEndUserJWT(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	users := newStubUserRepo()
	users.seed(projectID, activeUser(userID))
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, users, nil)

	token := signToken(t, jwtparser.Claims{
		UserID:    userID,
		ProjectID: projectID,
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	p, err := v.ValidateToken(ctx, token)
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindEndUser, p.ActorKind)
	require.True(t, p.HasRole("users"))
}

func TestValidator_ValidateEndUserJWT_RejectsRefreshToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := signToken(t, jwtparser.Claims{
		UserID:    "user-1",
		ProjectID: "proj-1",
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeRefresh,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, nil, nil)
	_, err := v.ValidateToken(ctx, token)
	requireCode(t, err, codes.Unauthenticated)
}

func TestValidator_ValidateAdminProjectAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := &stubAdminProjectRepo{access: map[string]map[string]struct{}{
		"admin-1": {"proj-allowed": {}},
	}}
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, repo, nil, nil, nil, nil)

	require.NoError(t, v.ValidateAdminProjectAccess(ctx, &shared.Principal{
		ActorKind:       shared.ActorKindAdmin,
		AdminID:         "admin-1",
		ProjectID:       "proj-allowed",
		IsPlatformAdmin: false,
	}))
	requireCode(t, v.ValidateAdminProjectAccess(ctx, &shared.Principal{
		ActorKind: shared.ActorKindAdmin,
		AdminID:   "admin-1",
		ProjectID: "proj-denied",
	}), codes.PermissionDenied)
	require.NoError(t, v.ValidateAdminProjectAccess(ctx, &shared.Principal{
		ActorKind:       shared.ActorKindAdmin,
		AdminID:         "admin-1",
		ProjectID:       "proj-denied",
		IsPlatformAdmin: true,
	}))
	require.NoError(t, v.ValidateAdminProjectAccess(ctx, &shared.Principal{
		ActorKind: shared.ActorKindAdmin,
		ActorID:   "admin-1",
		ProjectID: "proj-allowed",
	}))
}

func TestValidator_EndUserJWT_CorruptExpireAtFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	sessionID := idgen.UUID().String()
	users := newStubUserRepo()
	users.seed(projectID, activeUser(userID))
	sessions := newStubSessionRepo()
	sessions.seed(projectID, &domainauth.Session{ID: sessionID, UserID: userID})
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, sessions, users, nil)

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
	requireCode(t, err, codes.Unauthenticated)
}

func signSessionCookie(projectID, sessionID string) string {
	codec := auth.NewSessionCookieCodec(string(jwtparser.DeriveKey(testJWTSecret, jwtparser.PurposeSessionCookie)))
	return codec.Sign(projectID, sessionID)
}

func TestValidator_SessionCookie_Valid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	sessionID := idgen.UUID().String()
	users := newStubUserRepo()
	users.seed(projectID, activeUser(userID))
	sessions := newStubSessionRepo()
	sessions.seed(projectID, &domainauth.Session{ID: sessionID, UserID: userID, ExpireAt: time.Now().Add(time.Hour)})
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, sessions, users, nil)

	p, err := v.ValidateCredential(ctx, signSessionCookie(projectID, sessionID), shared.CredentialTypeSession)
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindEndUser, p.ActorKind)
	require.Equal(t, sessionID, p.SessionID)
}

func TestValidator_SessionCookie_CorruptExpireAtFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	sessionID := idgen.UUID().String()
	users := newStubUserRepo()
	users.seed(projectID, activeUser(userID))
	sessions := newStubSessionRepo()
	sessions.seed(projectID, &domainauth.Session{ID: sessionID, UserID: userID})
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, sessions, users, nil)

	_, err := v.ValidateCredential(ctx, signSessionCookie(projectID, sessionID), shared.CredentialTypeSession)
	requireCode(t, err, codes.Unauthenticated)
}

func TestValidator_CrossPurposeTokenRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admin := &projects.Admin{ID: "admin-1", Email: "admin@torchwood.local", Role: "owner"}
	admins := &stubAdminRepo{admins: map[string]*projects.Admin{admin.ID: admin}}
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, admins, &stubAdminProjectRepo{}, nil, nil, nil, nil)

	// A token signed with the raw master secret must no longer validate.
	rawToken, err := jwtparser.Generate([]byte(testJWTSecret), jwtparser.Claims{
		UserID:    admin.ID,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	_, err = v.ValidateToken(ctx, rawToken)
	requireCode(t, err, codes.Unauthenticated)
}

// oneTimeJWTValidator 组装带一次性 token 存储的 validator（end-user 路径），
// 用户文档使用调用方传入的 projectID/userID。
func oneTimeJWTValidator(t *testing.T, store domainauth.OneTimeTokenStore, projectID, userID string) *auth.Validator {
	t.Helper()
	users := newStubUserRepo()
	users.seed(projectID, activeUser(userID))
	return auth.NewValidatorWithOneTimeTokens(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, users, nil, store, nil)
}

func oneTimeJWTSign(t *testing.T, projectID, userID, jti string) string {
	t.Helper()
	token, err := jwtparser.Generate(jwtparser.DeriveKey(testJWTSecret, jwtparser.PurposeEndUserJWT), jwtparser.Claims{
		TokenID:   jti,
		UserID:    userID,
		Username:  "user@example.com",
		ActorKind: "end_user",
		ProjectID: projectID,
		TokenType: jwtparser.TokenTypeAccess,
		OneTime:   true,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
	})
	require.NoError(t, err)
	return token
}

// TestValidator_OneTimeJWT_SecondUseRejected（R05-P2-8）：同一一次性 JWT
// 二次使用必须 Unauthenticated；首次使用正常放行。
func TestValidator_OneTimeJWT_SecondUseRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	store := newMemOneTimeTokenStore()
	v := oneTimeJWTValidator(t, store, projectID, userID)

	jti := idgen.UUID().String()
	// 模拟签发方 CreateJWT 的注册记录。
	ok, err := store.Register(ctx, domainauth.OneTimeJWTKeyPrefix+jti, "session-1", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	token := oneTimeJWTSign(t, projectID, userID, jti)

	p, err := v.ValidateToken(ctx, token)
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindEndUser, p.ActorKind)
	require.Equal(t, userID, p.UserID)

	// 同一 token 二次使用：消费记录已被 GETDEL，必须拒绝。
	_, err = v.ValidateToken(ctx, token)
	requireCode(t, err, codes.Unauthenticated)
}

// TestValidator_OneTimeJWT_WithoutConsumptionRecord：未登记消费记录（或已
// 过期/从未签发）的一次性 JWT 必须拒绝，防伪造。
func TestValidator_OneTimeJWT_WithoutConsumptionRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	v := oneTimeJWTValidator(t, newMemOneTimeTokenStore(), projectID, userID)

	token := oneTimeJWTSign(t, projectID, userID, idgen.UUID().String())
	_, err := v.ValidateToken(ctx, token)
	requireCode(t, err, codes.Unauthenticated)
}

// TestValidator_OneTimeJWT_WithoutStoreFailsClosed：验证侧未装配一次性存储时
// 一次性 JWT 一律拒绝（fail-closed），不得静默放行。
func TestValidator_OneTimeJWT_WithoutStoreFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	v := oneTimeJWTValidator(t, nil, projectID, userID)

	token := oneTimeJWTSign(t, projectID, userID, idgen.UUID().String())
	_, err := v.ValidateToken(ctx, token)
	requireCode(t, err, codes.Unauthenticated)
}

// TestValidator_NormalAccessToken_DoesNotTouchOneTimeStore：普通 access token
// 不得触发一次性消费（区分路径，防止误伤常规模拟）。
func TestValidator_NormalAccessToken_DoesNotTouchOneTimeStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectID := idgen.UUID().String()
	userID := idgen.UUID().String()
	store := newMemOneTimeTokenStore()
	v := oneTimeJWTValidator(t, store, projectID, userID)

	token := signToken(t, jwtparser.Claims{
		TokenID:   idgen.UUID().String(),
		UserID:    userID,
		ProjectID: projectID,
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	_, err := v.ValidateToken(ctx, token)
	require.NoError(t, err)
	require.Zero(t, store.consumed, "normal access token must not consume one-time records")
}
