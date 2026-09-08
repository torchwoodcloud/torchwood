package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/documentdb"
	"github.com/torchwooddev/torchwood/internal/infra/projectschema"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// platformAdminCtx 返回携带平台 admin principal 的上下文（M7 后 CreateProject
// 仅允许平台 admin，测试需显式注入）。
func platformAdminCtx(ctx context.Context) context.Context {
	return contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:         "admin-1",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: true,
	})
}

func TestProjects_CreateProject_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	p, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{
		ID:          "txapp",
		Name:        "Transactional App",
		Description: "integration test",
	})
	require.NoError(t, err)
	require.NotEmpty(t, p.ID)
	t.Cleanup(func() {
		_ = projectsUC.DeleteProjectInternal(ctx, p.ID)
	})

	coll, err := docDB.GetCollection(ctx, p.ID, databases.SystemDatabaseID, "users")
	require.NoError(t, err)
	require.Nil(t, coll, "cut 后 catalog 无 sentinel users")

	def, err := docDB.GetDatabase(ctx, p.ID, "default")
	require.NoError(t, err)
	require.NotNil(t, def, "CreateProject 应建第一业务库 default")

	sentinel, err := docDB.GetDatabase(ctx, p.ID, databases.SystemDatabaseID)
	require.NoError(t, err)
	require.Nil(t, sentinel, "cut 后 catalog 无 database_id='_'")

	for _, rel := range []string{
		"public.document_indexes",
		"public.document_attributes",
		"public.document_collections",
		"public.document_databases",
	} {
		var reg any
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass(?)`, rel).Scan(&reg), rel)
		require.Nil(t, reg, "D-7: %s 已删除", rel)
	}

	projectCatalog, err := db.NewSelect().Model((*model.DocumentCollection)(nil)).
		Where("project_id = ? AND is_system = TRUE", p.ID).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, projectCatalog, "cut 后 catalog 无 is_system 行")

	var staticUsers any
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass(?)`, "tw_"+p.ID+".users").Scan(&staticUsers))
	require.NotNil(t, staticUsers)
}

func TestProjects_CreateProject_RequiresPlatformAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	// API key 主体（ActorKind=service）被拒。
	apiKeyCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:     "key-1",
		ActorKind:   shared.ActorKindService,
		ProjectID:   "some-project",
		Roles:       []string{"keys"},
		Permissions: []string{"databases.write"},
	})
	_, err := projectsUC.CreateProject(apiKeyCtx, CreateProjectCommand{ID: "hacked", Name: "Hacked App"})
	require.Error(t, err)

	// 受限 admin（非平台 admin）被拒。
	viewerCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:   "admin-2",
		ActorKind: shared.ActorKindAdmin,
		UserID:    "admin-2",
		Roles:     []string{"viewer"},
	})
	_, err = projectsUC.CreateProject(viewerCtx, CreateProjectCommand{ID: "viewer", Name: "Viewer App"})
	require.Error(t, err)
}

func TestProjects_CreateProject_RejectsInvalidID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	for _, id := range []string{"", "Bad_Name", "my-shop", "1shop", "MyShop"} {
		_, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{ID: id, Name: "App"})
		require.Error(t, err, id)
	}
}

// TestProjects_CreateProject_RejectsOrphanDataPlane（T-R1 护栏，2026-09-08
// dev 事故复刻）：tw_<id> schema 存在而项目行缺失（控制面被重置、数据面
// 幸存）时，CreateProject 必须显式拒绝——静默重建会让新 internal_id 与
// 数据面烤死的 _tenant DEFAULT 失配（全部读空 + 写后回读 500）。
func TestProjects_CreateProject_RejectsOrphanDataPlane(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	schemaMgr := projectschema.NewSchemaManager(db)
	projectsUC := NewProjects(repo, docDB, db, schemaMgr, nil)

	// 第一纪元：正常建项目（数据面就位）。
	p, err := projectsUC.CreateProjectInternal(ctx, CreateProjectCommand{ID: "txorphan", Name: "Orphan Era"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = projectsUC.DeleteProjectInternal(ctx, p.ID) })

	// 复刻事故：带外删除项目行（仅控制面），数据面 schema 幸存。
	require.NoError(t, repo.DeleteProject(ctx, p.ID))
	exists, err := schemaMgr.Exists(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, exists, "复刻前提：数据面 schema 必须仍存在")

	// 护栏：同 ID 重建必须 FailedPrecondition，且不再写入项目行。
	_, err = projectsUC.CreateProjectInternal(ctx, CreateProjectCommand{ID: p.ID, Name: "Recreated"})
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "orphan data plane")
	row, err := repo.GetProject(ctx, p.ID)
	require.NoError(t, err)
	require.Nil(t, row, "护栏拒绝后不得残留项目行")

	// Exists 对不存在的项目恒 false（护栏不误伤全新创建）。
	exists, err = schemaMgr.Exists(ctx, "nosuchproject")
	require.NoError(t, err)
	require.False(t, exists)
}

func TestProjects_CreateProject_RollsBackOnFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	projectID := fmt.Sprintf("rb%x", time.Now().UnixNano())
	p := &projects.Project{
		ID:        projectID,
		Name:      fmt.Sprintf("Rollback Test %d", time.Now().UnixNano()),
		Status:    "active",
		Settings:  map[string]any{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := repo.CreateProject(txCtx, p); err != nil {
			return err
		}
		if err := projectschema.NewSchemaManager(db).Ensure(txCtx, p.ID); err != nil {
			return err
		}
		if err := docDB.CreateDatabase(txCtx, p.ID, "default", "default"); err != nil {
			return err
		}
		return fmt.Errorf("simulated failure")
	})
	require.Error(t, err)

	got, err := repo.GetProject(ctx, projectID)
	require.NoError(t, err)
	require.Nil(t, got)

	var ns any
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regnamespace(?)`, "tw_"+projectID).Scan(&ns))
	require.Nil(t, ns)
}

// createTestProject 直接经 repo 落库一个项目（UpdateProject 不需要 docDB）。
func createTestProject(t *testing.T, repo projects.Repository, id, name string) *projects.Project {
	t.Helper()
	p := &projects.Project{
		ID:        id,
		Name:      name,
		Status:    "active",
		Settings:  map[string]any{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.CreateProject(context.Background(), p))
	return p
}

// restrictedAdminCtx 返回携带非平台 admin principal 的上下文（绑定 projectID）。
func restrictedAdminCtx(ctx context.Context, projectID string) context.Context {
	return contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:         "admin-2",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: false,
		ProjectID:       projectID,
	})
}

func strPtr(s string) *string { return &s }

func TestProjects_UpdateProject_PlatformAdminSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	created := createTestProject(t, repo, "alpha", "Alpha App")
	time.Sleep(2 * time.Millisecond) // 保证 updated_at 严格递增

	got, err := projectsUC.UpdateProject(platformAdminCtx(ctx), UpdateProjectCommand{
		ProjectID:   "alpha",
		Name:        strPtr("Alpha Renamed"),
		Description: strPtr("new description"),
	})
	require.NoError(t, err)
	require.Equal(t, "Alpha Renamed", got.Name)
	require.Equal(t, "new description", got.Description)
	require.True(t, got.UpdatedAt.After(created.UpdatedAt), "updated_at 必须单调递增")

	// 断言 repo 层落库。
	persisted, err := repo.GetProject(ctx, "alpha")
	require.NoError(t, err)
	require.NotNil(t, persisted)
	require.Equal(t, "Alpha Renamed", persisted.Name)
	require.Equal(t, "new description", persisted.Description)
	require.True(t, persisted.UpdatedAt.After(created.UpdatedAt))
}

func TestProjects_UpdateProject_RestrictedAdminOwnProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	createTestProject(t, repo, "own", "Own App")
	got, err := projectsUC.UpdateProject(restrictedAdminCtx(ctx, "own"), UpdateProjectCommand{
		ProjectID:   "own",
		Description: strPtr("updated by restricted admin"),
	})
	require.NoError(t, err)
	require.Equal(t, "updated by restricted admin", got.Description)
}

func TestProjects_UpdateProject_RestrictedAdminOtherProjectNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	createTestProject(t, repo, "own", "Own App")
	createTestProject(t, repo, "other", "Other App")

	// 非平台 admin 越权更新他人项目 → NotFound（防存在性探测，与 GetProject 一致）。
	_, err := projectsUC.UpdateProject(restrictedAdminCtx(ctx, "own"), UpdateProjectCommand{
		ProjectID: "other",
		Name:      strPtr("Hacked"),
	})
	require.Equal(t, codes.NotFound, status.Code(err))

	// 无绑定项目（ProjectID 为空）同样拒绝。
	_, err = projectsUC.UpdateProject(restrictedAdminCtx(ctx, ""), UpdateProjectCommand{
		ProjectID: "own",
		Name:      strPtr("Hacked"),
	})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestProjects_UpdateProject_ProjectNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	_, err := projectsUC.UpdateProject(platformAdminCtx(ctx), UpdateProjectCommand{
		ProjectID: "missing",
		Name:      strPtr("Nope"),
	})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestProjects_UpdateProject_NothingToUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	// 前置检查：name 与 description 均未提供 → InvalidArgument（先于取数/越权）。
	_, err := projectsUC.UpdateProject(platformAdminCtx(ctx), UpdateProjectCommand{ProjectID: "alpha"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "nothing to update")
}

func TestProjects_UpdateProject_EmptyID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	_, err := projectsUC.UpdateProject(platformAdminCtx(ctx), UpdateProjectCommand{
		Name: strPtr("Whatever"),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestProjects_UpdateProject_BlankNameRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	createTestProject(t, repo, "alpha", "Alpha App")

	// 有意收紧：编辑场景拒绝空白名（严格于 CreateProject 的空名回落默认 id）。
	_, err := projectsUC.UpdateProject(platformAdminCtx(ctx), UpdateProjectCommand{
		ProjectID: "alpha",
		Name:      strPtr("   "),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "name is required")
}

func TestProjects_UpdateProject_NameCollision(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	createTestProject(t, repo, "alpha", "Alpha App")
	createTestProject(t, repo, "beta", "Beta App")

	// 改名撞名 → InvalidArgument（不落库，避免依赖 DB unique violation → 500）。
	_, err := projectsUC.UpdateProject(platformAdminCtx(ctx), UpdateProjectCommand{
		ProjectID: "beta",
		Name:      strPtr("Alpha App"),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "project name already exists")

	persisted, err := repo.GetProject(ctx, "beta")
	require.NoError(t, err)
	require.Equal(t, "Beta App", persisted.Name)
}

func TestProjects_CreateProject_RejectsLongDescription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	// 口径 a：CreateProject 与 UpdateProject 对 description 施加同一上限 512。
	_, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{
		ID:          "longdesc",
		Name:        "Long Description App",
		Description: strings.Repeat("x", 513),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "description")

	// 恰好 512 字符放行。
	p, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{
		ID:          "boundary",
		Name:        "Boundary App",
		Description: strings.Repeat("x", 512),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = projectsUC.DeleteProjectInternal(ctx, p.ID)
	})
}

func TestProjects_DeleteProject_DropsSchemas(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	p, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{
		ID:              "delme",
		Name:            "Delete Me",
		FirstDatabaseID: "app",
	})
	require.NoError(t, err)

	require.NoError(t, bunrepo.NewUserRepository(db).Insert(ctx, p.ID, &users.User{
		ID:     "gone-user",
		Email:  "gone@torchwood.local",
		Name:   "Gone",
		Status: users.StatusActive,
	}))

	require.NoError(t, projectsUC.DeleteProject(platformAdminCtx(ctx), p.ID))

	got, err := repo.GetProject(ctx, p.ID)
	require.NoError(t, err)
	require.Nil(t, got)

	var projectNS, appNS any
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regnamespace(?)`, "tw_delme").Scan(&projectNS))
	require.Nil(t, projectNS)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regnamespace(?)`, "tw_delme_app").Scan(&appNS))
	require.Nil(t, appNS)
}

func TestProjects_DeleteProject_RequiresPlatformAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	p, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{
		ID:   "delauth",
		Name: "Delete Auth",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = projectsUC.DeleteProjectInternal(ctx, p.ID)
	})

	err = projectsUC.DeleteProject(ctx, p.ID)
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	apiKeyCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:     "key-1",
		ActorKind:   shared.ActorKindService,
		ProjectID:   p.ID,
		Roles:       []string{"keys"},
		Permissions: []string{"projects.write"},
	})
	err = projectsUC.DeleteProject(apiKeyCtx, p.ID)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	viewerCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:   "admin-2",
		ActorKind: shared.ActorKindAdmin,
		UserID:    "admin-2",
		Roles:     []string{"viewer"},
	})
	err = projectsUC.DeleteProject(viewerCtx, p.ID)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	err = projectsUC.DeleteProject(platformAdminCtx(ctx), "missing")
	require.Equal(t, codes.NotFound, status.Code(err))

	got, err := repo.GetProject(ctx, p.ID)
	require.NoError(t, err)
	require.NotNil(t, got, "被拒删除不得动项目")
}

// TestProjects_DeleteProject_CleansPublicRows 锁死设计 §4.3 第 3 步：
// 级联删除必须清理 public 控制面里该项目的全部派生行（outbox / outbox_dead /
// api_keys / admin_projects / provider_resource_index）。
// M5 C7（审计留存不变量）：audit_logs 不在清理面——项目删除不得追溯抹除
// 历史审计轨迹，测试反向断言审计行保留。
func TestProjects_DeleteProject_CleansPublicRows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil)

	p, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{
		ID:   "delrows",
		Name: "Delete Public Rows",
	})
	require.NoError(t, err)

	// 预置 public 控制面行（原生 SQL：字段最小集，只验证 deletePublicProjectRows 的谓词）。
	seed := []string{
		`INSERT INTO document_events_outbox (event_id, project_id, topic, payload)
		 VALUES ('evt-1', 'delrows', 'databases.app.collections.posts', '{}')`,
		`INSERT INTO document_events_outbox_dead (event_id, project_id, topic, payload, attempts, created_at)
		 VALUES ('evt-2', 'delrows', 'databases.app.collections.posts', '{}', 3, NOW())`,
		`INSERT INTO api_keys (id, project_id, name, secret_hash)
		 VALUES ('key-1', 'delrows', 'seed', 'hash-1')`,
		`INSERT INTO audit_logs (id, project_id, actor_id, actor_kind, action, status)
		 VALUES ('aud-1', 'delrows', 'admin-1', 'admin', 'DeleteProject', 'success')`,
		`INSERT INTO admins (id, email, password_hash) VALUES ('adm-1', 'delrows@t.local', 'x')
		 ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO admin_projects (admin_id, project_id) VALUES ('adm-1', 'delrows')
		 ON CONFLICT (admin_id, project_id) DO NOTHING`,
		`INSERT INTO provider_resource_index (provider, kind, provider_ref, project_id)
		 VALUES ('stripe', 'payment_session', 'cs_seed_1', 'delrows')`,
	}
	for _, q := range seed {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM admins WHERE id = 'adm-1'`)
	})

	require.NoError(t, projectsUC.DeleteProjectInternal(ctx, p.ID))

	for _, table := range []string{
		"document_events_outbox",
		"document_events_outbox_dead",
		"api_keys",
		"admin_projects",
		"provider_resource_index",
	} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE project_id = 'delrows'`).Scan(&n), table)
		require.Zero(t, n, "%s rows must be cleaned by DeleteProjectInternal", table)
	}

	// M5 C7：审计行必须保留（留存不变量）。
	var auditRows int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE project_id = 'delrows'`).Scan(&auditRows))
	require.Equal(t, 1, auditRows, "audit_logs 必须随项目删除保留（审计留存不变量）")
}

// TestProjects_ListProjects_MemberGrantedProjects（B1）：member 在 admin_projects
// 有 Grant 时 ListProjects 非空；无 Grant 的 member 列表为空且 Get 其它项目 NotFound。
func TestProjects_ListProjects_MemberGrantedProjects(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	adminProjectRepo := bunrepo.NewAdminProjectRepository(db)
	adminRepo := bunrepo.NewAdminRepository(db)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), adminProjectRepo)
	require.NoError(t, adminRepo.CreateAdmin(ctx, &projects.Admin{
		ID:           "adm-member-1",
		Email:        "member-grant@test.local",
		PasswordHash: "x",
		Role:         "member",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}))

	owned, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{
		ID:   "memberapp",
		Name: "Member App",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = projectsUC.DeleteProjectInternal(ctx, owned.ID) })
	other, err := projectsUC.CreateProject(platformAdminCtx(ctx), CreateProjectCommand{
		ID:   "otherapp",
		Name: "Other App",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = projectsUC.DeleteProjectInternal(ctx, other.ID) })

	// 无 Grant：member 的 ProjectID 不会被授权层绑定（这里以未绑定模拟），列表为空。
	unboundCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:   "member-1",
		ActorKind: shared.ActorKindAdmin,
		AdminID:   "adm-member-1",
		Roles:     []string{"member"},
	})
	list, info, err := projectsUC.ListProjects(unboundCtx, 10, "")
	require.NoError(t, err)
	require.Empty(t, list)
	require.Zero(t, info.TotalCount)

	// Grant 后（等价于授权层校验通过并绑定 ProjectID）：列表包含该项目；
	// 但 Get 其它未授权项目仍 NotFound。
	require.NoError(t, adminProjectRepo.GrantProjectAccess(ctx, "adm-member-1", owned.ID))
	memberCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:   "member-1",
		ActorKind: shared.ActorKindAdmin,
		AdminID:   "adm-member-1",
		ProjectID: owned.ID,
		Roles:     []string{"member"},
	})
	list, info, err = projectsUC.ListProjects(memberCtx, 10, "")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, 1, info.TotalCount)
	require.Equal(t, owned.ID, list[0].ID)
	got, err := projectsUC.GetProject(memberCtx, owned.ID)
	require.NoError(t, err)
	require.Equal(t, owned.ID, got.ID)
	_, err = projectsUC.GetProject(memberCtx, other.ID)
	require.Equal(t, codes.NotFound, status.Code(err))
}
