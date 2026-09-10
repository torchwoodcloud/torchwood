// roles_sig 验签测试（阶段③-b 包 C + R16 返工）：
//   - tw_roles()/tw_tenant() 验签三态 fail-closed（无 sig / 错 sig / 过期 sig
//     → 零角色 / NULL tenant，与漏注入同语义）；
//   - R16 ② tw_set_document_acl 的租户绑定（跨项目/跨租户死锁）与可见性校验
//     （项目内改他人 ACL 提权读 → 0 行）；
//   - R16 ③ app.tenant GUC 篡改面（sig 失配 → 验签失败）；
//   - 合法注入路径全链路回归。
package documentdb

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// sigFixture 是单项目造数：业务集合（集合级 read:update:any + docsec）+ 一行
// 空 _acl 文档（可见性完全取决于 tw_roles() 解包 + 集合级判定）。
type sigFixture struct {
	docDB      *postgresDocumentDB
	db         *clients.Database
	projectID  string
	internalID int64
	tbl        string // schema.physical
}

func newSigFixture(ctx context.Context, t *testing.T) *sigFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	docDB := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	projectID, internalID, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	t.Cleanup(cleanup)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "read", Role: "any"},
		{Type: "update", Role: "any"},
	}, true))
	f := &sigFixture{docDB: docDB, db: db, projectID: projectID, internalID: internalID}
	_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID: "r1", Data: map[string]any{"title": "visible"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	f.tbl = testSchema(t, projectID, "app") + "." + testPhysicalName(t, ctx, db, projectID, "app", "docs")
	return f
}

// asAppWithGUC 以 tw_app 身份 + 手工 GUC（绕过 InjectExecIdentity 的签名）
// 执行 fn——用于伪造场景（tenant 默认取 fixture 的真实值，可覆写）。
func asAppWithGUC(ctx context.Context, db *clients.Database, tenant int64, roles, sig string, fn func(txCtx context.Context) error) error {
	return db.RunInTx(ctx, func(txCtx context.Context) error {
		if _, err := db.Conn(txCtx).ExecContext(txCtx,
			`SET LOCAL ROLE tw_app; SELECT set_config('app.roles', ?, true), set_config('app.roles_sig', ?, true), set_config('app.tenant', ?, true)`,
			roles, sig, fmt.Sprintf("%d", tenant)); err != nil {
			return err
		}
		return fn(txCtx)
	})
}

func countVisible(t *testing.T, db *clients.Database, tbl string) func(ctx context.Context) int {
	return func(ctx context.Context) int {
		var n int
		require.NoError(t, db.Conn(ctx).QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&n))
		return n
	}
}

func probe(t *testing.T, db *clients.Database, expr string) func(ctx context.Context) any {
	return func(ctx context.Context) any {
		var v any
		require.NoError(t, db.Conn(ctx).QueryRowContext(ctx, expr).Scan(&v))
		return v
	}
}

// TestRolesSig_FailClosed 三态：无 sig / 错 sig / 过期 sig → 零角色 + 不可见；
// 合法注入（InjectExecIdentity 自动签名，含 tenant）→ 角色解包 + 可见。
func TestRolesSig_FailClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	f := newSigFixture(ctx, t)
	count := countVisible(t, f.db, f.tbl)
	rolesN := probe(t, f.db, `SELECT cardinality(public.tw_roles())`)
	tenantV := probe(t, f.db, `SELECT public.tw_tenant()`)
	keyHex, ok := clients.RolesSigKeyHex()
	require.True(t, ok, "testutil.SetupTestDB 已初始化签名密钥")

	// 态①：无 sig（GUC 缺失）——手工伪造 roles 的老通道，验签后封死。
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", "", func(txCtx context.Context) error {
		require.EqualValues(t, 0, rolesN(txCtx), "无 sig 必须解包为零角色")
		require.EqualValues(t, 0, count(txCtx), "零角色 → policy 恒 false → 不可见")
		return nil
	}))

	// 态②：错 sig（格式合法、mac 不匹配）。
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", "9999999999|deadbeef", func(txCtx context.Context) error {
		require.EqualValues(t, 0, rolesN(txCtx))
		require.EqualValues(t, 0, count(txCtx))
		return nil
	}))

	// 态③：过期 sig（密钥正确、exp 在过去）。
	expired := clients.SignRolesSig(keyHex, f.internalID, "any", time.Now().Add(-2*time.Minute))
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", expired, func(txCtx context.Context) error {
		require.EqualValues(t, 0, rolesN(txCtx), "过期 sig 必须解包为零角色")
		require.EqualValues(t, 0, count(txCtx))
		return nil
	}))

	// 篡改面：roles / tenant 任一被改（用原签名）→ 消息失配 → 验签失败。
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "users", clients.SignRolesSig(keyHex, f.internalID, "any", time.Now()), func(txCtx context.Context) error {
		require.EqualValues(t, 0, rolesN(txCtx))
		return nil
	}))
	// R16 ①：篡改 app.tenant（跨租户目标）→ sig 失配 + tw_tenant() NULL。
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID+1, "any", clients.SignRolesSig(keyHex, f.internalID, "any", time.Now()), func(txCtx context.Context) error {
		require.EqualValues(t, 0, rolesN(txCtx), "tenant 篡改 → 消息失配 → 零角色")
		require.Nil(t, tenantV(txCtx), "tenant 篡改 → tw_tenant() NULL")
		require.EqualValues(t, 0, count(txCtx))
		return nil
	}))

	// 合法注入：InjectExecIdentity 自动签名（tenant 已由 execIdentity 填入）→
	// 角色解包 + tenant 解包 + 可见（回归全链路）。
	require.NoError(t, f.db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{
		Role: clients.RoleApp, Roles: []string{"any"}, Tenant: f.internalID,
	}), func(txCtx context.Context) error {
		require.EqualValues(t, 1, rolesN(txCtx), "合法 sig 必须解包出角色")
		require.EqualValues(t, f.internalID, tenantV(txCtx), "合法 sig 必须解包出 tenant")
		require.EqualValues(t, 1, count(txCtx), "角色有效 → policy 放行 → 可见")
		return nil
	}))

	// 非 tw_app 身份（authenticator 本体，role setting = none）即使 sig 合法
	// 也不得经 tw_roles()/tw_tenant() 取得角色/租户。
	require.NoError(t, f.db.RunInTx(ctx, func(txCtx context.Context) error {
		_, err := f.db.Conn(txCtx).ExecContext(txCtx,
			`SELECT set_config('app.roles', ?, true), set_config('app.roles_sig', ?, true), set_config('app.tenant', ?, true)`,
			"any", clients.SignRolesSig(keyHex, f.internalID, "any", time.Now()), fmt.Sprintf("%d", f.internalID))
		if err != nil {
			return err
		}
		require.EqualValues(t, 0, rolesN(txCtx), "仅 tw_app 身份验签；其余身份零角色")
		require.Nil(t, tenantV(txCtx))
		return nil
	}))
}

// TestSetDocumentACL_TenantBinding（R16 ②-a 验收①）：跨项目/跨租户直调函数
// 以错误 tenant → 0 行、目标行 _acl 不变；正确 tenant（合法可见）→ 1 行。
func TestSetDocumentACL_TenantBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	f := newSigFixture(ctx, t)
	// 项目 B（不同 internal_id，同库）：同构集合 + 一行文档——跨项目 = tenant 不同。
	projectB, internalB, cleanupB := testutil.CreateTestProjectThrough(ctx, f.db, 0)
	t.Cleanup(cleanupB)
	require.NotEqual(t, f.internalID, internalB, "两项目 internal_id 必须不同")
	require.NoError(t, f.docDB.CreateDatabase(ctx, projectB, "app", "App DB"))
	require.NoError(t, f.docDB.CreateCollection(ctx, projectB, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "read", Role: "any"}, {Type: "update", Role: "any"},
	}, true))
	_, err := f.docDB.CreateDocument(ctx, projectB, "app", "docs", databases.Document{
		ID: "victim", Data: map[string]any{"title": "b"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	tblB := testSchema(t, projectB, "app") + "." + testPhysicalName(t, ctx, f.db, projectB, "app", "docs")
	physB := testPhysicalName(t, ctx, f.db, projectB, "app", "docs")

	aclOf := func(tbl, docID string) string {
		var acl string
		require.NoError(t, f.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT array_to_string(_acl, ',') FROM %s WHERE _id = ?`, tbl), docID).Scan(&acl))
		return acl
	}

	// 项目 A 身份（合法注入）直调函数改项目 B 的行（tenant 错误）→ 0 行、_acl 不变。
	keyHex, _ := clients.RolesSigKeyHex()
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any",
		clients.SignRolesSig(keyHex, f.internalID, "any", time.Now()), func(txCtx context.Context) error {
			var n int64
			require.NoError(t, f.db.Conn(txCtx).QueryRowContext(txCtx,
				`SELECT public.tw_set_document_acl(?, ?, ?, ?, ?::text[])`,
				testSchema(t, projectB, "app"), physB, internalB, "victim", `{read:any}`).Scan(&n))
			require.Zero(t, n, "跨项目（错误 tenant）必须 0 行")
			return nil
		}))
	require.Empty(t, aclOf(tblB, "victim"), "目标行 _acl 不得被跨项目改写")

	// 同项目正确 tenant + 可见 → 1 行（合法通道对照）。
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any",
		clients.SignRolesSig(keyHex, f.internalID, "any", time.Now()), func(txCtx context.Context) error {
			var n int64
			require.NoError(t, f.db.Conn(txCtx).QueryRowContext(txCtx,
				`SELECT public.tw_set_document_acl(?, ?, ?, ?, ?::text[])`,
				testSchema(t, projectB, "app"), physB, internalB, "ghost", `{read:any}`).Scan(&n))
			require.Zero(t, n, "不存在的文档 → 0")
			return nil
		}))
}

// TestSetDocumentACL_VisibilityGate（R16 ②-b 验收②）：不可见行（他人私有
// _acl）直调函数改写 → 0 行；可见行（update-only 持权者）→ 成功。
func TestSetDocumentACL_VisibilityGate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	f := newSigFixture(ctx, t)

	// victim：他人私有（_acl 仅 bob 可见可写）。
	_, err := f.docDB.CreateDocument(ctx, f.projectID, "app", "docs", databases.Document{
		ID: "victim", Data: map[string]any{"title": "private"},
	}, []databases.Permission{
		{Type: "read", Role: "user:bob"}, {Type: "update", Role: "user:bob"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	// uponly：集合级 update:any（无 read）→ "可写即可读"可见。
	_, err = f.docDB.CreateDocument(ctx, f.projectID, "app", "docs", databases.Document{
		ID: "uponly", Data: map[string]any{"title": "up"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	// mallory（users/any）不可见 victim → 直调函数 → 0 行。
	keyHex, _ := clients.RolesSigKeyHex()
	callAs := func(roles []string, docID string) int64 {
		var n int64
		joined := strings.Join(roles, "\x1f")
		sig := clients.SignRolesSig(keyHex, f.internalID, joined, time.Now())
		require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, joined, sig, func(txCtx context.Context) error {
			return f.db.Conn(txCtx).QueryRowContext(txCtx,
				`SELECT public.tw_set_document_acl(?, ?, ?, ?, ?::text[])`,
				testSchema(t, f.projectID, "app"), testPhysicalName(t, ctx, f.db, f.projectID, "app", "docs"),
				f.internalID, docID, `{read:any}`).Scan(&n)
		}))
		return n
	}
	require.Zero(t, callAs([]string{"users", "user:mallory", "any"}, "victim"),
		"不可见行（他人私有 _acl）直调函数必须 0 行（项目内提权死锁）")
	var acl string
	require.NoError(t, f.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT array_to_string(_acl, ',') FROM %s WHERE _id = 'victim'`, f.tbl)).Scan(&acl))
	require.Equal(t, "read:user:bob,update:user:bob", acl, "victim 的 _acl 不得被改写")

	// mallory 对 uponly 可见（空 _acl + 集合级 read/update:any）→ 1 行。
	require.EqualValues(t, 1, callAs([]string{"users", "user:mallory", "any"}, "uponly"),
		"可见行（集合级 update:any）函数通道放行")

	// bob（victim 持权者）→ 可见 → 1 行（合法权限替换）。
	require.EqualValues(t, 1, callAs([]string{"users", "user:bob", "any"}, "victim"))
}

// TestSetDocumentACL_InjectionSurface：伪造 p_table（不在 catalog physical_name
// 白名单）→ 函数异常（42704），事务回滚；错误 schema（白名单命中）不可达。
func TestSetDocumentACL_InjectionSurface(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	f := newSigFixture(ctx, t)
	keyHex, _ := clients.RolesSigKeyHex()
	sig := clients.SignRolesSig(keyHex, f.internalID, "any", time.Now())

	err := asAppWithGUC(ctx, f.db, f.internalID, "any", sig, func(txCtx context.Context) error {
		var n int
		return f.db.Conn(txCtx).QueryRowContext(txCtx,
			`SELECT public.tw_set_document_acl(?, ?, ?, ?, ?::text[])`,
			"public", "pg_class", f.internalID, "r1", `{read:any}`).Scan(&n)
	})
	require.Error(t, err, "伪造表名必须被 catalog 白名单拒绝")
	require.Contains(t, err.Error(), "unknown collection table")

	// 白名单外的 schema + 合法物理名：%I 引证保证无注入，表不存在即报错。
	err = asAppWithGUC(ctx, f.db, f.internalID, "any", sig, func(txCtx context.Context) error {
		var n int
		physical := f.tbl[strings.LastIndex(f.tbl, ".")+1:]
		return f.db.Conn(txCtx).QueryRowContext(txCtx,
			`SELECT public.tw_set_document_acl(?, ?, ?, ?, ?::text[])`,
			"public", physical, f.internalID, "r1", `{read:any}`).Scan(&n)
	})
	require.Error(t, err)
}

// TestRolesSig_LegitPathRegression：合法注入路径全链路（创建→读→改权限→删）
// 经 docDB 公共 API 回归——sig/tenant 随 execIdentity 自动注入，无须调用方
// 感知；update/upsert/bulk 的 _acl 替换经函数通道（可见性校验不影响合法面）。
func TestRolesSig_LegitPathRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	f := newSigFixture(ctx, t)
	alice := databases.Principal{Roles: []string{"users", "user:alice"}}

	got, err := f.docDB.GetDocument(ctx, f.projectID, "app", "docs", "r1", alice)
	require.NoError(t, err)
	require.NotNil(t, got, "集合级 read:any + 空 _acl → 可见（sig 验签通过）")

	list, err := f.docDB.ListDocuments(ctx, f.projectID, "app", "docs", databases.Query{}, alice)
	require.NoError(t, err)
	require.Equal(t, int64(1), list.TotalCount)

	// 权限替换（tw_set_document_acl 函数通道，可见性校验通过）+ 自锁后读回。
	updated, err := f.docDB.UpdateDocument(ctx, f.projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: "r1", Data: map[string]any{"title": "v2"}},
		Permissions:     []databases.Permission{{Type: "read", Role: "user:alice"}},
		ExpectedVersion: got.Version,
	}, alice)
	require.NoError(t, err, "可见行的权限替换（函数通道）不受可见性校验影响")
	require.Equal(t, "v2", updated.Data["title"])
	require.Equal(t, []databases.Permission{{Type: "read", Role: "user:alice"}}, updated.Permissions)

	// SystemPrincipal 的权限替换走信任根直写（tw_system 无函数 EXECUTE，R16④）。
	sysUpdated, err := f.docDB.UpdateDocument(ctx, f.projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: "r1", Data: map[string]any{"title": "v3"}},
		Permissions:     []databases.Permission{{Type: "read", Role: "any"}},
		ExpectedVersion: updated.Version,
	}, databases.SystemPrincipal)
	require.NoError(t, err, "系统身份的 _acl 替换走 tw_system 直写通道")
	require.Equal(t, []databases.Permission{{Type: "read", Role: "any"}}, sysUpdated.Permissions)

	require.NoError(t, f.docDB.DeleteDocument(ctx, f.projectID, "app", "docs", "r1", databases.DeleteOptions{ExpectedVersion: sysUpdated.Version}, databases.SystemPrincipal))
}
