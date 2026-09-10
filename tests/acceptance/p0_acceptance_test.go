package acceptance_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"github.com/torchwoodcloud/torchwood/internal/api/clientgrpc"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	"github.com/torchwoodcloud/torchwood/internal/app/client"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	infrAuth "github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	inframessaging "github.com/torchwoodcloud/torchwood/internal/infra/messaging"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

func TestP0_Section6_AdminProjectAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, projectCleanup := testutil.CreateTestProject(ctx, db)
	defer projectCleanup()

	cfg := &config.AppConfig{}
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	env, err := testutil.NewInterceptorEnv(db, cfg, docDB)
	require.NoError(t, err)

	owner, ownerCleanup := testutil.CreateTestAdmin(ctx, db, "owner")
	defer ownerCleanup()
	viewer, viewerCleanup := testutil.CreateTestAdmin(ctx, db, "viewer")
	defer viewerCleanup()

	ownerToken, err := testutil.SignAdminToken(cfg, owner)
	require.NoError(t, err)
	viewerToken, err := testutil.SignAdminToken(cfg, viewer)
	require.NoError(t, err)

	adminMD := func(token string) metadata.MD {
		return metadata.Pairs(
			"authorization", "Bearer "+token,
			"X-Torchwood-Project", projectID,
		)
	}

	// §6.8 owner with X-Torchwood-Project can access Server API.
	err = env.InvokeUnary(ctx, testutil.MethodListUsers, adminMD(ownerToken))
	require.NoError(t, err)

	// §6.7 viewer without admin_projects gets PermissionDenied.
	err = env.InvokeUnary(ctx, testutil.MethodListUsers, adminMD(viewerToken))
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.PermissionDenied, st.Code())

	// §6.9 viewer granted project access can call Server API.
	require.NoError(t, testutil.GrantAdminProject(ctx, db, viewer.ID, projectID))
	err = env.InvokeUnary(ctx, testutil.MethodListUsers, adminMD(viewerToken))
	require.NoError(t, err)
}

func TestP0_Section7_AuditLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, projectCleanup := testutil.CreateTestProject(ctx, db)
	defer projectCleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	cfg := &config.AppConfig{}
	env, err := testutil.NewInterceptorEnv(db, cfg, docDB)
	require.NoError(t, err)

	apiSecret, keyCleanup := testutil.CreateTestAPIKey(ctx, db, projectID, []string{"users"})
	defer keyCleanup()

	before, err := env.AuditLogCount(ctx)
	require.NoError(t, err)

	// §7.1 authenticated call writes audit_logs row.
	err = env.InvokeUnary(ctx, testutil.MethodListUsers, metadata.Pairs("x-api-key", apiSecret))
	require.NoError(t, err)

	after, err := env.AuditLogCount(ctx)
	require.NoError(t, err)
	require.Equal(t, before+1, after)

	// §7.2 latest row has action/status/actor fields.
	log, err := env.LatestAuditLog(ctx)
	require.NoError(t, err)
	require.Equal(t, testutil.MethodListUsers, log.Action)
	require.Equal(t, "success", log.Status)
	require.NotEmpty(t, log.ActorID)
	require.NotEmpty(t, log.ActorKind)

	// §7.3 admin request with X-Torchwood-Project records project_id.
	owner, ownerCleanup := testutil.CreateTestAdmin(ctx, db, "owner")
	defer ownerCleanup()
	ownerToken, err := testutil.SignAdminToken(cfg, owner)
	require.NoError(t, err)
	require.NoError(t, env.InvokeUnary(ctx, testutil.MethodListUsers, metadata.Pairs(
		"authorization", "Bearer "+ownerToken,
		"X-Torchwood-Project", projectID,
	)))

	log, err = env.LatestAuditLog(ctx)
	require.NoError(t, err)
	require.Equal(t, projectID, log.ProjectID)

	// §7.4 public health must succeed; audit is best-effort and must not fail the request.
	err = env.InvokeUnary(ctx, testutil.MethodHealthCheck, metadata.Pairs())
	require.NoError(t, err)
}

func TestP0_Section8_AccessPermission(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, projectCleanup := testutil.CreateTestProject(ctx, db)
	defer projectCleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	cfg := &config.AppConfig{}
	env, err := testutil.NewInterceptorEnv(db, cfg, docDB)
	require.NoError(t, err)

	projectRepo := bunrepo.NewProjectRepository(db)
	account := newAcceptanceTestAccount(cfg, projectRepo, db)
	_, tokens, _, _, err := account.SignUp(ctx, client.SignUpCommand{
		ProjectID: projectID,
		Email:     "access-perm@torchwood.local",
		Password:  "User@123456",
		Name:      "Access Perm",
	})
	require.NoError(t, err)

	userMD := metadata.Pairs("authorization", "Bearer "+tokens.AccessToken)

	// §8.1 Me requires users role ��� end-user token passes auth interceptor.
	err = env.InvokeUnary(ctx, testutil.MethodAccountMe, userMD)
	require.NoError(t, err)

	// §8.2 SignOut requires users role.
	err = env.InvokeUnary(ctx, testutil.MethodAccountSignOut, userMD)
	require.NoError(t, err)

	// §8.3 principal without users role is rejected on permission-gated methods.
	mockValidator := &principalValidator{
		principal: &shared.Principal{
			ActorKind:      shared.ActorKindEndUser,
			CredentialType: shared.CredentialTypeToken,
			ProjectID:      projectID,
			UserID:         "no-users-role",
			Roles:          []string{"guests"},
		},
	}
	policySet, err := domainauth.NewPolicySet([]domainauth.MethodPolicy{
		{Method: testutil.MethodAccountMe, Service: "/torchwood.client.v1.AccountService", Access: domainauth.AccessEndUser, Permissions: []string{"users"}},
	})
	require.NoError(t, err)
	authIC, err := interceptor.NewAuthInterceptor(mockValidator, policySet)
	require.NoError(t, err)
	ctx = metadata.NewIncomingContext(ctx, userMD)
	_, err = authIC.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
		FullMethod: testutil.MethodAccountMe,
	}, func(ctx context.Context, req any) (any, error) { return nil, nil })
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.PermissionDenied, st.Code())
}

func TestP0_Section9_DynamicDocuments(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, projectCleanup := testutil.CreateTestProject(ctx, db)
	defer projectCleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	cfg := &config.AppConfig{}
	projectRepo := bunrepo.NewProjectRepository(db)
	account := newAcceptanceTestAccount(cfg, projectRepo, db)
	usersRepo := bunrepo.NewUserRepository(db)
	sessionRepo := bunrepo.NewSessionRepository(db)
	roles := client.NewUserRoles(usersRepo, bunrepo.NewMembershipRepository(db))
	usersUC := appserver.NewUsers(projectRepo, infrAuth.NewSessionService(cfg, sessionRepo, roles, nil), db, usersRepo, sessionRepo, bunrepo.NewGroupRepository(db), bunrepo.NewMembershipRepository(db))

	const email = "dsl-query@torchwood.local"
	signedUp, _, _, _, err := account.SignUp(ctx, client.SignUpCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123456",
		Name:      "DSL Query",
	})
	require.NoError(t, err)

	// §9.1 system users collection contains registered user (API key / keys role).
	docs, total, _, err := usersUC.ListUsers(ctx, projectID, databases.Query{}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.GreaterOrEqual(t, total, int64(1))
	found := false
	for _, doc := range docs {
		if doc.ID == signedUp.ID {
			found = true
			break
		}
	}
	require.True(t, found, "registered user should appear in users list")

	// §9.2 query filter returns only matching user.
	filtered, filteredTotal, _, err := usersUC.ListUsers(ctx, projectID, databases.Query{
		Queries: []string{`equal("email","` + email + `")`},
	}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Equal(t, int64(1), filteredTotal)
	require.Len(t, filtered, 1)
	require.Equal(t, email, filtered[0].Data["email"])

	// §9.4 业务集合仍按 _perms 过滤（系统 users 已不是 collection）。
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "app"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))
	privateNote, err := docDB.CreateDocument(ctx, projectID, "app", "notes", databases.Document{
		Data: map[string]any{"title": "Private"},
	}, []databases.Permission{
		{Type: "read", Role: "user:alice"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)

	aliceList, err := docDB.ListDocuments(ctx, projectID, "app", "notes", databases.Query{
		Queries: []string{`equal("$id","` + privateNote.ID + `")`},
	}, databases.Principal{Roles: []string{"user:alice"}})
	require.NoError(t, err)
	require.Len(t, aliceList.Documents, 1)

	bobList, err := docDB.ListDocuments(ctx, projectID, "app", "notes", databases.Query{
		Queries: []string{`equal("$id","` + privateNote.ID + `")`},
	}, databases.Principal{Roles: []string{"user:bob"}})
	require.NoError(t, err)
	require.Len(t, bobList.Documents, 0)
}

type principalValidator struct {
	principal *shared.Principal
}

func (v *principalValidator) Authenticate(ctx context.Context, req shared.AuthnRequest) (*shared.Principal, error) {
	if _, _, err := shared.ParseAuthnRequest(req); err != nil {
		return nil, err
	}
	return v.principal, nil
}

func (v *principalValidator) ValidateToken(ctx context.Context, token string) (*shared.Principal, error) {
	return v.ValidateCredential(ctx, token, shared.CredentialTypeToken)
}

func (v *principalValidator) ValidateCredential(ctx context.Context, raw string, credentialType shared.CredentialType) (*shared.Principal, error) {
	return v.principal, nil
}

func (v *principalValidator) ValidateAdminProjectAccess(ctx context.Context, principal *shared.Principal) error {
	return nil
}

// newAcceptanceTestAccount 以真实依赖装配 Account 用例（等价原
// client.NewTestAccount；该符号已随 Round4 J4-2 收敛回包内测试文件，
// 不再对包外可见）。
func newAcceptanceTestAccount(cfg *config.AppConfig, projectRepo projects.Repository, db *clients.Database) *client.Account {
	usersRepo := bunrepo.NewUserRepository(db)
	sessionRepo := bunrepo.NewSessionRepository(db)
	identities := bunrepo.NewIdentityRepository(db)
	roles := client.NewUserRoles(usersRepo, bunrepo.NewMembershipRepository(db))
	sessions := infrAuth.NewSessionService(cfg, sessionRepo, roles, nil)
	mailer := inframessaging.NewMailer(cfg)
	sms := inframessaging.NewSMSService(cfg)
	return client.NewAccount(cfg, projectRepo, nil, nil, sessions, nil, nil, nil, nil, nil, nil, mailer, sms, nil, roles, nil, nil, nil, nil, usersRepo, identities, sessionRepo, nil, nil, nil, nil)
}

// ——P2 客户端调用面 × P0 执行身份 端到端验收（分层集成）——
//
// 函数容器执行器（docker/dispatcher）在 CI 不可用，用桩 executor 替代容器：
// 桩在「执行进行中」回调里模拟函数代码行为——以注入的
// `Bearer $TW_EXECUTION_TOKEN` 经真实拦截器链回访 Server API（assets grant）。
// 其余每一环都是真实组件：端用户 JWT 真签发真校验、bun 仓储落真库、Redis
// token 服务/限频器跑 miniredis、账本落真表。
type acceptanceExecutor struct {
	onExecute func(execToken string)
	called    int
}

func (m *acceptanceExecutor) Build(context.Context, string, string, string) error { return nil }
func (m *acceptanceExecutor) Execute(_ context.Context, e domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	m.called++
	if m.onExecute != nil {
		m.onExecute(e.Env["TW_EXECUTION_TOKEN"])
	}
	return &domainfunctions.ExecutionResult{StatusCode: 0, Response: `{"ok":true}`}, nil
}
func (m *acceptanceExecutor) RemoveImage(context.Context, string, string) error { return nil }

type acceptanceQueue struct{}

func (acceptanceQueue) Trim(context.Context, string, int64) error     { return nil }
func (acceptanceQueue) Enqueue(context.Context, string, []byte) error { return nil }
func (acceptanceQueue) Dequeue(context.Context, string, time.Duration) ([]byte, string, error) {
	return nil, "", nil
}
func (acceptanceQueue) Ack(context.Context, string, string) error { return nil }

// 验收：终端用户 JWT invoke 一个 client_callable + client_per_user_limit 的
// 函数 → 函数内以执行身份 token 发放资产（Server API assets grant）成功，
// 账本 operator 溯源到 function + invoking user 双维；同用户第二次 invoke
// 撞限频，返回 FUNCTIONS.INVOKE_QUOTA_EXCEEDED。
func TestP0_Section10_ClientInvokeExecutionIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, projectCleanup := testutil.CreateTestProject(ctx, db)
	defer projectCleanup()

	cfg := &config.AppConfig{}
	projectRepo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	// 端用户真实注册（JWT 经真实 session/users 校验链）。
	account := newAcceptanceTestAccount(cfg, projectRepo, db)
	user, tokens, _, _, err := account.SignUp(ctx, client.SignUpCommand{
		ProjectID: projectID,
		Email:     "client-invoke@torchwood.local",
		Password:  "User@123456",
		Name:      "Client Invoke",
	})
	require.NoError(t, err)
	userMD := metadata.Pairs("authorization", "Bearer "+tokens.AccessToken)

	// Redis（miniredis）：执行 token 服务 + 客户端限频器，与生产同一实现。
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	tokenSvc := infrafunctions.NewRedisExecutionTokenService(rdb)
	quota := infrafunctions.NewClientQuotaLimiter(rdb)

	// 种 client_callable + per-user 限频（limit=1/day）+ assets:write scope 的
	// 函数与 ready 部署（真实 DB 行；容器镜像与构建不在本验收范围）。
	fnRepo := bunrepo.NewFunctionRepository(db)
	now := time.Now()
	fn := &domainfunctions.Function{
		ID: "fn_reward", ProjectID: projectID, Name: "reward", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		ClientCallable: true, ClientPerUserLimit: 1, ClientLimitWindow: domainfunctions.ClientLimitWindowDay,
		DeclaredScopes: []string{"assets:write"},
		CreatedAt:      now, UpdatedAt: now,
	}
	require.NoError(t, fnRepo.CreateFunction(ctx, fn))
	require.NoError(t, fnRepo.CreateDeployment(ctx, &domainfunctions.Deployment{
		ID: "dep_reward", FunctionID: fn.ID, ProjectID: projectID, Size: 1024,
		Status: domainfunctions.DeploymentStatusReady, CreatedAt: now, UpdatedAt: now,
	}))

	// 资产定义（Server principal 安排步骤；真实资产用例 + 真库）。
	assetsUC := appassets.NewAssets(
		db,
		bunrepo.NewAssetDefRepository(db),
		bunrepo.NewAssetHoldingRepository(db),
		bunrepo.NewAssetLedgerRepository(db),
		infraevents.NewEventOutbox(db),
		nil,
		projectRepo,
	)
	serverCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		ProjectID:      projectID,
		APIKeyID:       "k1",
	})
	_, err = assetsUC.CreateDef(serverCtx, appassets.CreateDefCommand{Code: "gold", Name: "Gold", Class: domainassets.ClassCurrency})
	require.NoError(t, err)

	// 拦截器链（validator 装配执行 token 服务）+ 桩 executor：执行中模拟
	// 函数代码回访 Server API 发放资产。
	env, err := testutil.NewInterceptorEnvWithExecutionTokens(db, cfg, docDB, tokenSvc)
	require.NoError(t, err)

	var execToken string
	executor := &acceptanceExecutor{onExecute: func(token string) {
		execToken = token
		// 函数容器内行为：Authorization: Bearer $TW_EXECUTION_TOKEN →
		// POST /v1/server/assets:grant。
		err := env.InvokeUnaryHandler(context.Background(), testutil.MethodAssetsGrant,
			metadata.Pairs("authorization", "Bearer "+token),
			func(grantCtx context.Context, _ any) (any, error) {
				return assetsUC.Grant(grantCtx, domainassets.GrantCommand{
					OwnerType:      domainassets.OwnerTypeUser,
					OwnerID:        user.ID,
					DefCode:        "gold",
					Quantity:       1,
					IdempotencyKey: "reward-" + user.ID,
				})
			})
		require.NoError(t, err, "函数内以执行身份调 Server API 应成功")
	}}
	fnUC := appfunctions.NewFunctionsWithClientQuota(cfg, executor, fnRepo, acceptanceQueue{}, nil, nil, appfunctions.Semaphores{}, tokenSvc, nil, quota)

	// ① 终端用户 JWT invoke（真实拦截器链 → END_USER 策略门 → handler）。
	err = env.InvokeUnaryHandler(ctx, testutil.MethodInvokeFunction, userMD,
		func(invokeCtx context.Context, _ any) (any, error) {
			return clientgrpc.NewFunctionsService(fnUC).InvokeFunction(invokeCtx, &clientv1.InvokeFunctionRequest{
				FunctionId: fn.ID,
				Data:       `{"level":3}`,
			})
		})
	require.NoError(t, err)
	require.NotEmpty(t, execToken, "执行身份 token 应注入函数容器")

	// ② 账本 operator = function + invoking user 双维（平台背书的调用者
	// 身份，非 TW_DATA 客户端自报）。
	entries, err := bunrepo.NewAssetLedgerRepository(db).ListByOwner(ctx, projectID, domainassets.OwnerTypeUser, user.ID, "", 10, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	var operator struct {
		ActorKind string `json:"actor_kind"`
		ActorID   string `json:"actor_id"`
		UserID    string `json:"user_id"`
	}
	require.NoError(t, json.Unmarshal(entries[0].Operator, &operator))
	require.Equal(t, "execution", operator.ActorKind)
	require.Equal(t, fn.ID, operator.ActorID, "账本应溯源到函数")
	require.Equal(t, user.ID, operator.UserID, "账本应溯源到调用用户")

	// ③ 同用户第二次 invoke：per-user 限频（limit=1）→
	// ResourceExhausted + FUNCTIONS.INVOKE_QUOTA_EXCEEDED。
	err = env.InvokeUnaryHandler(ctx, testutil.MethodInvokeFunction, userMD,
		func(invokeCtx context.Context, _ any) (any, error) {
			return clientgrpc.NewFunctionsService(fnUC).InvokeFunction(invokeCtx, &clientv1.InvokeFunctionRequest{FunctionId: fn.ID})
		})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, st.Code())
	var reason *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			reason = ei
		}
	}
	require.NotNil(t, reason)
	require.Equal(t, "FUNCTIONS.INVOKE_QUOTA_EXCEEDED", reason.Reason)
	require.Equal(t, 1, executor.called, "超限请求不得触达执行器")
}
