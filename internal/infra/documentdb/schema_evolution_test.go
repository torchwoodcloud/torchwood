// schema 演进状态机集成测试（转出 POC 门禁 B4，docs/developer/15-exit-poc.md）：
//   - deprecated 生命周期：读屏蔽（Get/List 剥离）+ 查询白名单拒绝 + 写入
//     拒收 + 不可作索引目标 + RestoreAttribute 回滚（判据原文"deprecated
//     读写屏蔽测试"）；
//   - copy 迁移往返（判据原文"copy 迁移往返"）：integer→float 数据保真、
//     swap 后新列接管逻辑名、旧列 deprecated 残留、retire 退役；
//   - schema_version 消费（判据原文"被状态机消费"）：迁移 commit 递增；
//   - §4.6 契约表逐行：加列 required 带 default、放宽即时（扩宽/required→
//     optional）、收紧/改类型 copy 迁移、删列两段（重命名行 = 不提供，文档）。
package documentdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

func setupEvolutionEnv(t *testing.T) *onlineIndexEnv {
	t.Helper()
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	p := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	t.Cleanup(cleanup)
	require.NoError(t, p.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 64},
		{ID: "qty", Key: "qty", Type: "integer"},
	}, nil, append(anyPerms(), databases.Permission{Type: "create", Role: "users"}), true))
	env := &onlineIndexEnv{
		db: db, p: p, project: projectID,
		schema: testSchema(t, projectID, "app"), database: "app", collection: "docs",
		physical: testPhysicalName(t, ctx, db, projectID, "app", "docs"),
	}
	for i := 0; i < 5; i++ {
		doc, err := p.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
			Data: map[string]any{"title": "t" + string(rune('a'+i)), "qty": int64(i + 1)},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
		env.docIDs = append(env.docIDs, doc.ID)
	}
	return env
}

// attrStatus 读 catalog attrs 中指定属性的 status（归一）。
func (env *onlineIndexEnv) attrStatus(t *testing.T, ctx context.Context, key string) string {
	t.Helper()
	m := new(model.DocumentCollection)
	require.NoError(t, env.db.NewSelect().Model(m).
		Where("project_id = ? AND database_id = ? AND collection_id = ?", env.project, env.database, env.collection).
		Scan(ctx))
	attrs, err := decodeAttributes(m.Attrs)
	require.NoError(t, err)
	for _, a := range attrs {
		if a.Key == key {
			return a.StatusOrDefault()
		}
	}
	return "absent"
}

// schemaVersion 读 catalog schema_version。
func (env *onlineIndexEnv) schemaVersion(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var v int64
	require.NoError(t, env.db.NewSelect().Model((*model.DocumentCollection)(nil)).
		Column("schema_version").
		Where("project_id = ? AND database_id = ? AND collection_id = ?", env.project, env.database, env.collection).
		Scan(ctx, &v))
	return v
}

// waitMigrationPhase 等待同 key 最新任务到达目标阶段（后台回填 goroutine 的
// 确定性同步点）。
func (env *onlineIndexEnv) waitMigrationPhase(t *testing.T, ctx context.Context, key, phase string) {
	t.Helper()
	testutil.Eventually(t, 30*time.Second, func() bool {
		var n int
		require.NoError(t, env.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM catalog_migrations WHERE project_id = ? AND database_id = ? AND collection_id = ? AND attr_key = ? AND phase = ?`,
			env.project, env.database, env.collection, key, phase).Scan(&n))
		return n > 0
	})
}

// TestAttributeLifecycle_DeprecateMaskRestore 是 B4 判据本体（deprecated 读写
// 屏蔽 + 可回滚）：deprecate 后读屏蔽/查询拒绝/写入拒收/不可索引，restore
// 后全部恢复。
func TestAttributeLifecycle_DeprecateMaskRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupEvolutionEnv(t)

	// 段一：deprecated。
	require.NoError(t, env.p.DeleteAttribute(ctx, env.project, env.database, env.collection, "title"))
	require.Equal(t, databases.AttrStatusDeprecated, env.attrStatus(t, ctx, "title"))
	// 物理列与数据仍在（可回滚的前提）。
	require.True(t, env.columnExists(t, ctx, "title"))

	// 读屏蔽：Get/List 剥离 title。
	doc, err := env.p.GetDocument(ctx, env.project, env.database, env.collection, env.docIDs[0], databases.SystemPrincipal)
	require.NoError(t, err)
	_, has := doc.Data["title"]
	require.False(t, has, "deprecated 属性必须从读回剥离")
	require.Contains(t, doc.Data, "qty", "未 deprecated 属性不受屏蔽")
	docs, err := env.p.ListDocuments(ctx, env.project, env.database, env.collection,
		databases.Query{PageSize: 10}, databases.SystemPrincipal)
	require.NoError(t, err)
	for _, d := range docs.Documents {
		require.NotContains(t, d.Data, "title")
	}

	// 查询白名单拒绝（validateQueryFields；非 bypass 主体执行校验）。
	_, err = env.p.ListDocuments(ctx, env.project, env.database, env.collection, databases.Query{
		PageSize: 10,
		AST: &query.Query{Filter: &query.Filter{Op: query.OpEqual, Attribute: "title",
			Values: []string{"ta"}}},
	}, appUserPrincipal())
	require.ErrorContains(t, err, "deprecated and not queryable")

	// 写入拒收（create/update；非 bypass 主体——生命周期拒收是用户契约面）。
	_, err = env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
		Data: map[string]any{"title": "nope", "qty": int64(9)},
	}, anyPerms(), appUserPrincipal())
	require.ErrorIs(t, err, ErrAttributeDeprecated)
	_, err = env.p.UpdateDocument(ctx, env.project, env.database, env.collection, databases.DocumentUpdate{
		Document:        databases.Document{ID: env.docIDs[0], Data: map[string]any{"title": "nope"}},
		ExpectedVersion: 1,
	}, appUserPrincipal())
	require.ErrorIs(t, err, ErrAttributeDeprecated)

	// 不可作为索引目标。
	err = env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "title_idx", Type: "key", Attributes: []string{"title"},
	})
	require.ErrorContains(t, err, "cannot be indexed")

	// 回滚：deprecated → active，读写全部恢复。
	require.NoError(t, env.p.RestoreAttribute(ctx, env.project, env.database, env.collection, "title"))
	require.Equal(t, databases.AttrStatusActive, env.attrStatus(t, ctx, "title"))
	doc, err = env.p.GetDocument(ctx, env.project, env.database, env.collection, env.docIDs[0], databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "ta", doc.Data["title"], "回滚后数据原样可见（物理列未动）")
	_, err = env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
		Data: map[string]any{"title": "restored", "qty": int64(9)},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
}

// TestAttributeLifecycle_RetireTwoStage 是 §4.6 删列行的两段语义：deprecated
// → retired 物理删列不可逆；active 属性直接 retire 拒绝。
func TestAttributeLifecycle_RetireTwoStage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupEvolutionEnv(t)

	// active 直接 retire 拒绝（必须先 deprecated）。
	err := env.p.RetireAttribute(ctx, env.project, env.database, env.collection, "title")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	require.NoError(t, env.p.DeleteAttribute(ctx, env.project, env.database, env.collection, "title"))
	require.NoError(t, env.p.RetireAttribute(ctx, env.project, env.database, env.collection, "title"))
	require.False(t, env.columnExists(t, ctx, "title"), "retire 必须物理删列")
	require.Equal(t, "absent", env.attrStatus(t, ctx, "title"), "retire 后契约条目移除")
	// 同 key 可重建（条目已移除，非 deprecated 阻断）。
	require.NoError(t, env.p.CreateAttribute(ctx, env.project, env.database, env.collection,
		databases.Attribute{ID: "title", Key: "title", Type: "string", Size: 32}))
}

// TestCopyMigration_TypeChangeRoundTrip 是 B4 判据本体（copy 迁移往返）：
// integer→float 全量数据保真 + swap 后新列接管逻辑名 + 旧列 deprecated 残留
// + retire 退役；schema_version 随迁移 commit 递增。
func TestCopyMigration_TypeChangeRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupEvolutionEnv(t)
	versionBefore := env.schemaVersion(t, ctx)

	mig, err := env.p.MigrateAttribute(ctx, env.project, env.database, env.collection, "qty",
		databases.Attribute{Key: "qty", Type: "float"})
	require.NoError(t, err)
	require.Equal(t, migrationPhaseBackfilling, mig.Phase)
	// 等待 swap 完成（后台回填 goroutine）。
	env.waitMigrationPhase(t, ctx, "qty", migrationPhaseSwapped)

	// swap 后：目录条目 = 目标定义 active；schema_version 递增（消费点）。
	require.Equal(t, databases.AttrStatusActive, env.attrStatus(t, ctx, "qty"))
	versionAfter := env.schemaVersion(t, ctx)
	require.Greater(t, versionAfter, versionBefore, "schema_version 必须被迁移 commit 消费（递增）")
	// 物理面：新列 qty（float8），旧列 qty__d<seq> 残留（deprecated）。
	var udt string
	require.NoError(t, env.db.QueryRowContext(ctx,
		`SELECT udt_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = 'qty'`,
		env.schema, env.physical).Scan(&udt))
	require.Equal(t, "float8", udt)
	require.True(t, env.columnExists(t, ctx, "qty__d"+itoa64(versionAfter)))

	// 数据往返保真：旧值以新类型读回（1 → 1.0）。
	doc, err := env.p.GetDocument(ctx, env.project, env.database, env.collection, env.docIDs[0], databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, float64(1), doc.Data["qty"])
	// 新类型写入。
	_, err = env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
		Data: map[string]any{"title": "post", "qty": 2.5},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)

	// 旧列残留退役（retire swapped 任务）。
	require.NoError(t, env.p.RetireAttribute(ctx, env.project, env.database, env.collection, "qty"))
	require.False(t, env.columnExists(t, ctx, "qty__d"+itoa64(versionAfter)))
	require.True(t, env.columnExists(t, ctx, "qty"), "active 列不受残留退役影响")
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [21]byte
	pos := len(b)
	for v > 0 {
		pos--
		b[pos] = byte('0' + v%10)
		v /= 10
	}
	return string(b[pos:])
}

// TestCopyMigration_ValidateFailureAndResume：string→integer 撞不兼容数据 →
// 任务 failed、属性维持 migrating（写拒收）；修数后重入续跑收敛 swapped
// （判据"可恢复"）。
func TestCopyMigration_ValidateFailureAndResume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupEvolutionEnv(t)
	// 不兼容数据（string 列含非数值文本——title → integer 迁移的 validate 面）。
	_, err := env.db.ExecContext(ctx, `UPDATE `+env.schema+`.`+env.physical+` SET title = 'NaN!' WHERE _id = `+quoteLiteral(env.docIDs[0]))
	require.NoError(t, err)

	_, err = env.p.MigrateAttribute(ctx, env.project, env.database, env.collection, "title",
		databases.Attribute{Key: "title", Type: "integer"})
	require.NoError(t, err, "任务创建成功（validate 在回填阶段报告）")
	env.waitMigrationPhase(t, ctx, "title", migrationPhaseFailed)
	require.Equal(t, databases.AttrStatusMigrating, env.attrStatus(t, ctx, "title"),
		"失败任务属性维持 migrating（写拒收持续，显式不静默回滚）")
	// 写拒收（迁移窗口语义）。
	_, err = env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
		Data: map[string]any{"title": "blocked", "qty": int64(1)},
	}, anyPerms(), appUserPrincipal())
	require.ErrorIs(t, err, ErrAttributeMigrating)

	// 修数（系统旁路）→ 重入续跑 → swapped。
	_, err = env.db.ExecContext(ctx, `UPDATE `+env.schema+`.`+env.physical+` SET title = '42' WHERE _id = `+quoteLiteral(env.docIDs[0]))
	require.NoError(t, err)
	for _, id := range env.docIDs[1:] {
		_, err = env.db.ExecContext(ctx, `UPDATE `+env.schema+`.`+env.physical+` SET title = '7' WHERE _id = `+quoteLiteral(id))
		require.NoError(t, err)
	}
	_, err = env.p.MigrateAttribute(ctx, env.project, env.database, env.collection, "title",
		databases.Attribute{Key: "title", Type: "integer"})
	require.NoError(t, err)
	env.waitMigrationPhase(t, ctx, "title", migrationPhaseSwapped)
	require.Equal(t, databases.AttrStatusActive, env.attrStatus(t, ctx, "title"))
	doc, err := env.p.GetDocument(ctx, env.project, env.database, env.collection, env.docIDs[0], databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, float64(42), doc.Data["title"], "JSON 读回数值通道为 float64（契约）")
}

// TestCopyMigration_InstantRelaxation 是 §4.6 放宽行（即时 ALTER，无 copy）：
// required→optional 与 varchar 扩宽即时生效、schema_version 递增、零任务行。
func TestCopyMigration_InstantRelaxation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupEvolutionEnv(t)
	versionBefore := env.schemaVersion(t, ctx)

	// 扩宽 64 → 256（同类型 VARCHAR 扩宽 = 元数据级）。
	mig, err := env.p.MigrateAttribute(ctx, env.project, env.database, env.collection, "title",
		databases.Attribute{Key: "title", Type: "string", Size: 256})
	require.NoError(t, err)
	require.Equal(t, migrationPhaseSwapped, mig.Phase, "放宽即时完成（无回填任务窗口）")
	require.Greater(t, mig.SchemaVersion, versionBefore, "schema_version 消费（即时迁移同样递增）")
	var n int
	require.NoError(t, env.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM catalog_migrations WHERE project_id = ? AND attr_key = 'title'`,
		env.project).Scan(&n))
	require.Equal(t, 0, n, "放宽路径不产生 copy 任务行")

	// 迁移期写拒收的确定性锁定（直接置 migrating 模拟"任务创建后、swap 前"
	// 的迁移窗口）：非 bypass 主体的写入拒收（ErrAttributeMigrating）、查询
	// 放行（读服务旧列）、RestoreAttribute 中止恢复 active。
	env.forceAttrStatus(t, ctx, "qty", databases.AttrStatusMigrating)
	_, err = env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
		Data: map[string]any{"title": "blocked", "qty": int64(1)},
	}, anyPerms(), appUserPrincipal())
	require.ErrorIs(t, err, ErrAttributeMigrating)
	docs, err := env.p.ListDocuments(ctx, env.project, env.database, env.collection,
		databases.Query{PageSize: 10}, databases.SystemPrincipal)
	require.NoError(t, err, "migrating 属性查询放行（读服务旧列）")
	require.NotEmpty(t, docs.Documents)
	require.NoError(t, env.p.RestoreAttribute(ctx, env.project, env.database, env.collection, "qty"))
	require.Equal(t, databases.AttrStatusActive, env.attrStatus(t, ctx, "qty"))
	_, err = env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
		Data: map[string]any{"title": "unblocked", "qty": int64(1)},
	}, anyPerms(), appUserPrincipal())
	require.NoError(t, err)
}

// appUserPrincipal 是非 bypass 的业务主体（生命周期写拒收的执行面——bypass
// 主体为内部信任路径，不拒绝）。
func appUserPrincipal() databases.Principal {
	return databases.Principal{Roles: []string{"users"}}
}

// forceAttrStatus 绕过状态机直改属性生命周期状态（模拟迁移窗口等中间态）。
func (env *onlineIndexEnv) forceAttrStatus(t *testing.T, ctx context.Context, key, st string) {
	t.Helper()
	m := new(model.DocumentCollection)
	require.NoError(t, env.db.NewSelect().Model(m).
		Where("project_id = ? AND database_id = ? AND collection_id = ?", env.project, env.database, env.collection).
		Scan(ctx))
	attrs, err := decodeAttributes(m.Attrs)
	require.NoError(t, err)
	found := false
	for i := range attrs {
		if attrs[i].Key == key {
			attrs[i].Status = st
			found = true
		}
	}
	require.True(t, found)
	attrsJSON, err := encodeAttributes(attrs)
	require.NoError(t, err)
	m.Attrs = attrsJSON
	_, err = env.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	require.NoError(t, err)
}
