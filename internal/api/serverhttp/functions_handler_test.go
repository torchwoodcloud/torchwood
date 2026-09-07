package serverhttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appfunctions "github.com/torchwooddev/torchwood/internal/app/functions"
	"github.com/torchwooddev/torchwood/internal/domain/audit"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	domainusers "github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/infra/auth"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const functionsTestJWTSecret = "functions-handler-test-secret"

func functionsTestConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{Jwt: &config.Security_Jwt{Secret: functionsTestJWTSecret}},
	}
}

type functionsAPIKeyRepo struct {
	keys map[string]*projects.APIKey
}

func (r *functionsAPIKeyRepo) CreateAPIKey(context.Context, *projects.APIKey) error { return nil }
func (r *functionsAPIKeyRepo) GetAPIKey(context.Context, string, string) (*projects.APIKey, error) {
	return nil, nil
}
func (r *functionsAPIKeyRepo) GetAPIKeyBySecretHash(_ context.Context, hash string) (*projects.APIKey, error) {
	return r.keys[hash], nil
}
func (r *functionsAPIKeyRepo) ListAPIKeys(context.Context, string) ([]projects.APIKey, error) {
	return nil, nil
}
func (r *functionsAPIKeyRepo) DeleteAPIKey(context.Context, string, string) error { return nil }

type functionsAdminRepo struct {
	admins map[string]*projects.Admin
}

func (r *functionsAdminRepo) GetAdmin(_ context.Context, id string) (*projects.Admin, error) {
	return r.admins[id], nil
}
func (r *functionsAdminRepo) RevokeCredentials(context.Context, string, time.Time) error {
	return nil
}
func (r *functionsAdminRepo) GetAdminByEmail(context.Context, string) (*projects.Admin, error) {
	return nil, nil
}
func (r *functionsAdminRepo) ListAdmins(context.Context) ([]projects.Admin, error) { return nil, nil }
func (r *functionsAdminRepo) CreateAdmin(context.Context, *projects.Admin) error   { return nil }
func (r *functionsAdminRepo) UpdateAdmin(context.Context, *projects.Admin) error   { return nil }
func (r *functionsAdminRepo) DeleteAdmin(context.Context, string) error            { return nil }
func (r *functionsAdminRepo) CountAdminsByRole(context.Context, string) (int64, error) {
	return 1, nil
}
func (r *functionsAdminRepo) WithBootstrapLock(_ context.Context, _ int64, fn func(ctx context.Context) error) error {
	return fn(context.Background())
}

type functionsAdminProjectRepo struct{}

func (functionsAdminProjectRepo) HasProjectAccess(context.Context, string, string) (bool, error) {
	return true, nil
}
func (functionsAdminProjectRepo) GrantProjectAccess(context.Context, string, string) error {
	return nil
}
func (functionsAdminProjectRepo) ListProjectIDs(context.Context, string) ([]string, error) {
	return nil, nil
}

type functionsDocDB struct {
	users map[string]map[string]map[string]any
}

func (d *functionsDocDB) GetDocument(_ context.Context, projectID, _, collectionID, docID string, _ databases.Principal) (*databases.Document, error) {
	if collectionID == "users" {
		if data, ok := d.users[projectID][docID]; ok {
			return &databases.Document{ID: docID, Data: data}, nil
		}
	}
	return nil, nil
}
func (d *functionsDocDB) CreateDatabase(context.Context, string, string, string) error { return nil }
func (d *functionsDocDB) GetDatabase(context.Context, string, string) (*databases.Database, error) {
	return nil, nil
}
func (d *functionsDocDB) ListDatabases(context.Context, string) ([]databases.Database, error) {
	return nil, nil
}
func (d *functionsDocDB) DeleteDatabase(context.Context, string, string) error { return nil }
func (d *functionsDocDB) CreateCollection(context.Context, string, string, string, string, []databases.Attribute, []databases.Index, []databases.Permission, bool) error {
	return nil
}
func (d *functionsDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return nil, nil
}
func (d *functionsDocDB) ListCollections(context.Context, string, string, databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (d *functionsDocDB) DeleteCollection(context.Context, string, string, string) error { return nil }
func (d *functionsDocDB) UpdateCollection(context.Context, string, string, string, databases.CollectionPatch) error {
	return nil
}
func (d *functionsDocDB) CreateAttribute(context.Context, string, string, string, databases.Attribute) error {
	return nil
}
func (d *functionsDocDB) DeleteAttribute(context.Context, string, string, string, string) error {
	return nil
}
func (d *functionsDocDB) CreateIndex(context.Context, string, string, string, databases.Index) error {
	return nil
}
func (d *functionsDocDB) DeleteIndex(context.Context, string, string, string, string) error {
	return nil
}
func (d *functionsDocDB) CreateDocument(context.Context, string, string, string, databases.Document, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *functionsDocDB) UpdateDocument(context.Context, string, string, string, databases.DocumentUpdate, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *functionsDocDB) UpsertDocument(context.Context, string, string, string, databases.Document, []string, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *functionsDocDB) DeleteDocument(context.Context, string, string, string, string, databases.DeleteOptions, databases.Principal) error {
	return nil
}
func (d *functionsDocDB) ListDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (*databases.DocumentList, error) {
	return nil, nil
}
func (d *functionsDocDB) CountDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *functionsDocDB) AggregateDocuments(context.Context, string, string, string, databases.Query, []databases.AggregateSpec, string, databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (d *functionsDocDB) BulkUpdateDocuments(context.Context, string, string, string, []string, map[string]any, []databases.Permission, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *functionsDocDB) BulkDeleteDocuments(context.Context, string, string, string, []string, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *functionsDocDB) EnsureCatalog(context.Context, string) error { return nil }

func (*functionsDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (*functionsDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	panic("ListChanges: not implemented in test fake")
}

var _ databases.DocumentDB = (*functionsDocDB)(nil)

func functionsHashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func functionsSignToken(t *testing.T, claims jwtparser.Claims) string {
	t.Helper()
	purpose := jwtparser.PurposeEndUserJWT
	if claims.ActorKind == "admin" {
		purpose = jwtparser.PurposeAdminJWT
	}
	token, err := jwtparser.Generate(jwtparser.DeriveKey(functionsTestJWTSecret, purpose), claims)
	require.NoError(t, err)
	return token
}

func newFunctionsHandler(t *testing.T, validator *auth.Validator) *FunctionsHandler {
	t.Helper()
	h, err := NewFunctionsHandler(functionsTestConfig(), validator, nil, nil, nil, testPolicies())
	require.NoError(t, err)
	return h
}

func newFunctionsValidator(docDB *functionsDocDB) *auth.Validator {
	users := &functionsUserRepo{users: map[string]*domainusers.User{}}
	if docDB != nil {
		for _, byUser := range docDB.users {
			for id := range byUser {
				users.users[id] = &domainusers.User{ID: id, Status: domainusers.StatusActive}
			}
		}
	}
	return auth.NewValidator(functionsTestConfig(), &functionsAPIKeyRepo{}, nil, &functionsAdminRepo{}, &functionsAdminProjectRepo{}, nil, nil, users, nil)
}

type functionsUserRepo struct {
	users map[string]*domainusers.User
}

func (r *functionsUserRepo) GetByEmail(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *functionsUserRepo) GetByID(_ context.Context, _, id string) (*domainusers.User, error) {
	if r.users == nil {
		return nil, nil
	}
	u := r.users[id]
	if u == nil {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}
func (r *functionsUserRepo) GetByPhone(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *functionsUserRepo) Insert(context.Context, string, *domainusers.User) error { return nil }
func (r *functionsUserRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *functionsUserRepo) Delete(context.Context, string, string) error { return nil }
func (r *functionsUserRepo) List(context.Context, string, domainusers.ListFilter) (*domainusers.ListResult, error) {
	return &domainusers.ListResult{}, nil
}
func (r *functionsUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return nil
}

// TestFunctionsHandler_RejectsEndUserDeploymentUpload（F2-3）：端用户 Bearer JWT
// 上传 deployment 代码包必须 403 PermissionDenied。
func TestFunctionsHandler_RejectsEndUserDeploymentUpload(t *testing.T) {
	t.Parallel()

	projectID := "proj-1"
	userID := "user-1"
	docDB := &functionsDocDB{
		users: map[string]map[string]map[string]any{
			projectID: {userID: {"status": "active"}},
		},
	}
	validator := newFunctionsValidator(docDB)
	h := newFunctionsHandler(t, validator)

	token := functionsSignToken(t, jwtparser.Claims{
		UserID:    userID,
		ProjectID: projectID,
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.upload(rec, r, map[string]string{"functionId": "fn-1"})
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// TestFunctionsHandler_RejectsEndUserSessionCookie（F2-3）：端用户会话 cookie
// 同样被拒。
func TestFunctionsHandler_RejectsEndUserSessionCookie(t *testing.T) {
	t.Parallel()

	projectID := "proj-1"
	userID := "user-1"
	docDB := &functionsDocDB{
		users: map[string]map[string]map[string]any{
			projectID: {userID: {"status": "active"}},
		},
	}
	validator := newFunctionsValidator(docDB)
	h := newFunctionsHandler(t, validator)

	token := functionsSignToken(t, jwtparser.Claims{
		UserID:    userID,
		ProjectID: projectID,
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", nil)
	r.AddCookie(&http.Cookie{Name: "TORCHWOOD_session_proj-1", Value: token}) // #nosec G124 -- 客户端测试请求无需安全属性
	rec := httptest.NewRecorder()
	h.upload(rec, r, map[string]string{"functionId": "fn-1"})
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// TestFunctionsHandler_Authorize_AdminAndAPIKey（F2-3 对照）：admin 会话与带
// functions 写 scope 的 API Key 放行；无 scope 的 API Key 拒绝。
func TestFunctionsHandler_Authorize_AdminAndAPIKey(t *testing.T) {
	t.Parallel()

	admin := &projects.Admin{ID: "admin-1", Email: "admin@torchwood.local", Role: "owner"}
	validatorWithAdmin := auth.NewValidator(functionsTestConfig(), &functionsAPIKeyRepo{}, nil, &functionsAdminRepo{
		admins: map[string]*projects.Admin{admin.ID: admin},
	}, &functionsAdminProjectRepo{}, nil, nil, nil, nil)
	hAdmin := newFunctionsHandler(t, validatorWithAdmin)

	adminToken := functionsSignToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		Username:  admin.Email,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	t.Run("admin session allowed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", nil)
		r.Header.Set("Authorization", "Bearer "+adminToken)
		p, err := hAdmin.authorize(r)
		require.NoError(t, err)
		require.Equal(t, "admin", string(p.ActorKind))
	})

	t.Run("api key with functions scope allowed", func(t *testing.T) {
		repo := &functionsAPIKeyRepo{keys: map[string]*projects.APIKey{
			functionsHashSecret("fn-key-ok"): {
				ID:        "key-1",
				ProjectID: "proj-1",
				Scopes:    []string{"functions.write"},
				Enabled:   true,
			},
		}}
		v := auth.NewValidator(functionsTestConfig(), repo, nil, &functionsAdminRepo{}, &functionsAdminProjectRepo{}, nil, nil, nil, nil)
		h := newFunctionsHandler(t, v)
		r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", nil)
		r.Header.Set("X-Api-Key", "fn-key-ok")
		p, err := h.authorize(r)
		require.NoError(t, err)
		require.Equal(t, "service", string(p.ActorKind))
	})

	t.Run("api key without functions scope denied", func(t *testing.T) {
		repo := &functionsAPIKeyRepo{keys: map[string]*projects.APIKey{
			functionsHashSecret("fn-key-bad"): {
				ID:        "key-2",
				ProjectID: "proj-1",
				Scopes:    []string{"users"},
				Enabled:   true,
			},
		}}
		v := auth.NewValidator(functionsTestConfig(), repo, nil, &functionsAdminRepo{}, &functionsAdminProjectRepo{}, nil, nil, nil, nil)
		h := newFunctionsHandler(t, v)
		r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", nil)
		r.Header.Set("X-Api-Key", "fn-key-bad")
		_, err := h.authorize(r)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})
}

// TestFunctionsHandler_Authorize_ViewerMemberDenied（2026-08 评审 P0-3）：
// viewer/member 角色的 admin 会话不得上传部署代码包——对齐 gRPC 拦截器
// admin_roles.go 对 CreateDeployment 的 {owner, admin} 要求；此前 HTTP 路径
// 缺该检查，viewer 可部署任意代码。
func TestFunctionsHandler_Authorize_ViewerMemberDenied(t *testing.T) {
	t.Parallel()

	for _, role := range []string{"viewer", "member"} {
		t.Run(role+" denied", func(t *testing.T) {
			admin := &projects.Admin{ID: "admin-1", Email: "admin@torchwood.local", Role: role}
			validator := auth.NewValidator(functionsTestConfig(), &functionsAPIKeyRepo{}, nil, &functionsAdminRepo{
				admins: map[string]*projects.Admin{admin.ID: admin},
			}, &functionsAdminProjectRepo{}, nil, nil, nil, nil)
			h := newFunctionsHandler(t, validator)

			token := functionsSignToken(t, jwtparser.Claims{
				UserID:    admin.ID,
				Username:  admin.Email,
				ActorKind: "admin",
				TokenType: jwtparser.TokenTypeAccess,
				IssuedAt:  time.Now().Unix(),
				ExpiresAt: time.Now().Add(time.Hour).Unix(),
			})
			r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			_, err := h.authorize(r)
			require.Equal(t, codes.PermissionDenied, status.Code(err), "role %s 不得上传部署代码包", role)
		})
	}
}

// ---- Round3 H2-1：upload 必须把已鉴权 principal 注入 ctx 再调 CreateDeployment ----

// functionsTestRepo 是最小 FunctionRepo 桩：仅 GetFunction/CreateDeployment/
// UpdateDeployment 需要真实语义（其余方法测试路径用不到）。
type functionsTestRepo struct {
	fn *domainfunctions.Function
}

func (r *functionsTestRepo) CreateFunction(context.Context, *domainfunctions.Function) error {
	return nil
}
func (r *functionsTestRepo) GetFunction(_ context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	if r.fn != nil && r.fn.ProjectID == projectID && r.fn.ID == functionID {
		return r.fn, nil
	}
	return nil, nil
}
func (r *functionsTestRepo) ListFunctions(context.Context, string) ([]domainfunctions.Function, error) {
	return nil, nil
}
func (r *functionsTestRepo) UpdateFunction(context.Context, *domainfunctions.Function) error {
	return nil
}
func (r *functionsTestRepo) DeleteFunction(context.Context, string, string) error {
	return nil
}
func (r *functionsTestRepo) CreateDeployment(context.Context, *domainfunctions.Deployment) error {
	return nil
}
func (r *functionsTestRepo) GetDeployment(context.Context, string, string, string) (*domainfunctions.Deployment, error) {
	return nil, nil
}
func (r *functionsTestRepo) ListDeployments(context.Context, string, string) ([]domainfunctions.Deployment, error) {
	return nil, nil
}
func (r *functionsTestRepo) UpdateDeployment(context.Context, *domainfunctions.Deployment) error {
	return nil
}
func (r *functionsTestRepo) DeleteDeployment(context.Context, string, string, string) error {
	return nil
}
func (r *functionsTestRepo) SetVariables(context.Context, string, string, map[string]string) error {
	return nil
}
func (r *functionsTestRepo) GetVariables(context.Context, string, string) (map[string]string, error) {
	return nil, nil
}
func (r *functionsTestRepo) CreateExecution(context.Context, *domainfunctions.ExecutionRecord) error {
	return nil
}
func (r *functionsTestRepo) GetExecution(context.Context, string, string, string) (*domainfunctions.ExecutionRecord, error) {
	return nil, nil
}
func (r *functionsTestRepo) ListExecutions(context.Context, string, string, int) ([]domainfunctions.ExecutionRecord, error) {
	return nil, nil
}
func (r *functionsTestRepo) UpdateExecution(context.Context, *domainfunctions.ExecutionRecord) error {
	return nil
}
func (r *functionsTestRepo) RecoverOrphanExecutionsInProject(context.Context, string, time.Time, int) (int64, error) {
	return 0, nil
}
func (r *functionsTestRepo) PruneOldExecutionsInProject(context.Context, string, string, int) error {
	return nil
}
func (r *functionsTestRepo) TransitionExecutionStatus(context.Context, string, string, string, string, string) (bool, error) {
	return false, nil
}
func (r *functionsTestRepo) FailExecutionIfActive(context.Context, string, string, string, string) error {
	return nil
}

// functionsRecordingExecutor 记录 Build 收到的 ctx（CreateDeployment 内部
// buildDeployment 会把 handler 传入的 ctx 一路传到 executor），用于断言
// handler 是否把 principal 注入 ctx。
type functionsRecordingExecutor struct {
	buildCtx context.Context
	builds   int
}

func (e *functionsRecordingExecutor) Build(ctx context.Context, _, _, _ string) error {
	e.buildCtx = ctx
	e.builds++
	return nil
}
func (e *functionsRecordingExecutor) Execute(context.Context, domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	return nil, nil
}
func (e *functionsRecordingExecutor) RemoveImage(context.Context, string, string) error {
	return nil
}

// newUploadRequest 构造带 code 文件（PK\x03\x04 魔数）的 multipart 上传请求。
func newUploadRequest(t *testing.T, header map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("code", "code.zip")
	require.NoError(t, err)
	_, err = fw.Write([]byte("PK\x03\x04fake-zip-body"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return r
}

// TestFunctionsHandler_Upload_InjectsPrincipalIntoCtx：admin 会话与带
// functions.write 的 API Key 走完 upload 必须 201，且 executor 收到的 ctx
// 里能读到 principal（ActorKind 为 admin/service）。修复前 upload 用裸
// r.Context()，RequireServerPrincipal 在 use-case 入口直接 401，executor
// 不会被调、此测试失败——证明测的是根因。
func TestFunctionsHandler_Upload_InjectsPrincipalIntoCtx(t *testing.T) {
	projectID := "proj-1"
	admin := &projects.Admin{ID: "admin-1", Email: "admin@torchwood.local", Role: "owner"}

	t.Run("admin session", func(t *testing.T) {
		repo := &functionsTestRepo{fn: &domainfunctions.Function{
			ID: "fn-1", ProjectID: projectID, Runtime: "node-18.0", TimeoutSeconds: 15,
		}}
		executor := &functionsRecordingExecutor{}
		uc := appfunctions.NewFunctions(functionsTestConfig(), executor, repo, nil)
		validator := auth.NewValidator(functionsTestConfig(), &functionsAPIKeyRepo{}, nil, &functionsAdminRepo{
			admins: map[string]*projects.Admin{admin.ID: admin},
		}, &functionsAdminProjectRepo{}, nil, nil, nil, nil)
		h, err := NewFunctionsHandler(functionsTestConfig(), validator, uc, nil, nil, testPolicies())
		require.NoError(t, err)

		token := functionsSignToken(t, jwtparser.Claims{
			UserID:    admin.ID,
			Username:  admin.Email,
			ActorKind: "admin",
			TokenType: jwtparser.TokenTypeAccess,
			IssuedAt:  time.Now().Unix(),
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		r := newUploadRequest(t, map[string]string{
			"Authorization":       "Bearer " + token,
			"X-Torchwood-Project": projectID,
		})
		rec := httptest.NewRecorder()
		h.upload(rec, r, map[string]string{"functionId": "fn-1"})
		require.Equal(t, http.StatusCreated, rec.Code, "admin 上传应成功：%s", rec.Body.String())
		require.Equal(t, 1, executor.builds, "CreateDeployment 必须被调用（构建进入 executor）")
		p, ok := contexts.Principal(executor.buildCtx)
		require.True(t, ok, "executor 收到的 ctx 必须含 Principal")
		require.Equal(t, shared.ActorKindAdmin, p.ActorKind, "admin 会话的 ActorKind 必须为 admin")
	})

	t.Run("api key with functions.write", func(t *testing.T) {
		repo := &functionsTestRepo{fn: &domainfunctions.Function{
			ID: "fn-1", ProjectID: projectID, Runtime: "node-18.0", TimeoutSeconds: 15,
		}}
		executor := &functionsRecordingExecutor{}
		uc := appfunctions.NewFunctions(functionsTestConfig(), executor, repo, nil)
		validator := auth.NewValidator(functionsTestConfig(), &functionsAPIKeyRepo{
			keys: map[string]*projects.APIKey{
				functionsHashSecret("fn-key-ok"): {
					ID: "key-1", ProjectID: projectID, Scopes: []string{"functions.write"}, Enabled: true,
				},
			},
		}, nil, &functionsAdminRepo{}, &functionsAdminProjectRepo{}, nil, nil, nil, nil)
		h, err := NewFunctionsHandler(functionsTestConfig(), validator, uc, nil, nil, testPolicies())
		require.NoError(t, err)

		r := newUploadRequest(t, map[string]string{"X-Api-Key": "fn-key-ok"})
		rec := httptest.NewRecorder()
		h.upload(rec, r, map[string]string{"functionId": "fn-1"})
		require.Equal(t, http.StatusCreated, rec.Code, "API key 上传应成功：%s", rec.Body.String())
		require.Equal(t, 1, executor.builds, "CreateDeployment 必须被调用（构建进入 executor）")
		p, ok := contexts.Principal(executor.buildCtx)
		require.True(t, ok, "executor 收到的 ctx 必须含 Principal")
		require.Equal(t, shared.ActorKindService, p.ActorKind, "API key 的 ActorKind 必须为 service")
	})
}

// functionsAuditRepo 记录插入的审计条目（内存桩）。
type functionsAuditRepo struct {
	entries []*audit.Entry
}

func (r *functionsAuditRepo) Insert(_ context.Context, e *audit.Entry) error {
	r.entries = append(r.entries, e)
	return nil
}
func (r *functionsAuditRepo) ListByActor(context.Context, string, string, int) ([]audit.Entry, error) {
	return nil, nil
}

// TestFunctionsHandler_UploadWritesAudit（P2-6）：特权 HTTP 操作（代码包
// 上传）必须有持久审计轨迹——授权失败也落一条 PermissionDenied。
func TestFunctionsHandler_UploadWritesAudit(t *testing.T) {
	t.Parallel()

	projectID := "proj-1"
	userID := "user-1"
	docDB := &functionsDocDB{
		users: map[string]map[string]map[string]any{
			projectID: {userID: {"status": "active"}},
		},
	}
	validator := newFunctionsValidator(docDB)
	auditRepo := &functionsAuditRepo{}
	h, err := NewFunctionsHandler(functionsTestConfig(), validator, nil, auditRepo, nil, testPolicies())
	require.NoError(t, err)

	token := functionsSignToken(t, jwtparser.Claims{
		UserID:    userID,
		ProjectID: projectID,
		ActorKind: "end_user",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/server/functions/fn-1/deployments/code", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.upload(rec, r, map[string]string{"functionId": "fn-1"})
	require.Equal(t, http.StatusForbidden, rec.Code)

	require.Len(t, auditRepo.entries, 1, "授权失败必须落审计")
	entry := auditRepo.entries[0]
	require.Equal(t, "http.functions.deployment-upload", entry.Action)
	require.Equal(t, "PermissionDenied", entry.Status)
	// 授权失败路径无 principal（与 gRPC AuditInterceptor 一致），actor 字段为空。
}

// testPolicies 定义于 auth_test.go（同包共享）。
