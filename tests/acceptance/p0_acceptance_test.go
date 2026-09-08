package acceptance_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/api/interceptor"
	"github.com/torchwooddev/torchwood/internal/app/client"
	appserver "github.com/torchwooddev/torchwood/internal/app/server"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	infrAuth "github.com/torchwooddev/torchwood/internal/infra/auth"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/infra/documentdb"
	inframessaging "github.com/torchwooddev/torchwood/internal/infra/messaging"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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
