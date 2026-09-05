// RLS 判定执行点测试（阶段③包 C，redesign §3.3/§4.3/I1-I2）：SQL golden 矩阵
//（函数级，直接对 tw_can/tw_coll_allows/tw_visible 断言，CI 锁语义）+ 行为级
// 断言（经 policy）双层；EXPLAIN InitPlan 门禁 + 10 万行表 RLS 开/关相对基准。
package documentdb

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainevents "github.com/torchwooddev/torchwood/internal/domain/events"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/testutil"
)

// ---------------------------------------------------------------------------
// 函数级 golden 矩阵（I2）
// ---------------------------------------------------------------------------

func TestRLS_GoldenMatrix_TwCan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	can := func(acl any, roles any, typ string, collAllows bool) bool {
		t.Helper()
		var got bool
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT public.tw_can(?::text[], ?::text[], ?, ?)`,
			acl, roles, typ, collAllows).Scan(&got))
		return got
	}

	// 角色命中 × ACE 型。
	require.True(t, can(`{read:user:a}`, `{user:a,any}`, "read", false))
	require.False(t, can(`{read:user:a}`, `{user:b,any}`, "read", false))
	require.False(t, can(`{update:user:a}`, `{user:a,any}`, "read", false), "update ACE 不授予 read")
	// B14：key:<id> 角色对 policy 函数透明（任意角色串）——持有者命中、
	// 其他 key 不命中（per-key 隔离的函数级锚点）。
	require.True(t, can(`{read:key:k1}`, `{key:k1,any}`, "read", false))
	require.False(t, can(`{read:key:k1}`, `{key:k2,any}`, "read", false), "跨 key 不命中（per-key 私有）")
	require.True(t, can(`{write:key:k1}`, `{key:k1,any}`, "delete", false), "write 展开对 key:<id> 同构")
	// write 展开：create/update/delete 命中 write ACE；read 不命中。
	require.True(t, can(`{write:user:a}`, `{user:a,any}`, "update", false))
	require.True(t, can(`{write:user:a}`, `{user:a,any}`, "delete", false))
	require.True(t, can(`{write:user:a}`, `{user:a,any}`, "create", false))
	require.False(t, can(`{write:user:a}`, `{user:a,any}`, "read", false), "write 不隐含 read（单 op 语义）")
	// B1 空数组回退集合级。
	require.True(t, can(`{}`, `{any}`, "read", true))
	require.False(t, can(`{}`, `{any}`, "read", false))
	// 非空 ACE 覆盖集合级（文档级优先）。
	require.False(t, can(`{update:user:a}`, `{user:b,any}`, "read", true), "非空 _acl 关闭集合回退")
	// fail-closed：roles NULL / 空（Go nil → SQL NULL）。
	require.False(t, can(`{read:any}`, nil, "read", true))
	require.False(t, can(`{}`, nil, "read", true))
	require.False(t, can(`{}`, `{}`, "read", true))
}

func TestRLS_GoldenMatrix_TwCollAllows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	allows := func(perms string, roles any, typ string) bool {
		t.Helper()
		var got bool
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT public.tw_coll_allows(?::jsonb, ?::text[], ?)`,
			perms, roles, typ).Scan(&got))
		return got
	}

	coll := `[{"type":"read","role":"any"},{"type":"update","role":"users"},{"type":"write","role":"admins"}]`
	require.True(t, allows(coll, `{any}`, "read"))
	require.False(t, allows(coll, `{any}`, "update"), "集合级 update 不对 any 开放")
	require.True(t, allows(coll, `{users,any}`, "update"))
	require.True(t, allows(coll, `{admins}`, "delete"), "write 展开到 delete")
	require.True(t, allows(coll, `{admins}`, "create"))
	require.False(t, allows(coll, `{admins}`, "read"), "write 不隐含集合级 read（admins 不持 any，隔离断言）")
	require.False(t, allows(`[]`, `{any}`, "read"), "空集合权限恒 false")
	require.False(t, allows(coll, nil, "read"), "roles NULL fail-closed")
}

func TestRLS_GoldenMatrix_TwVisible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	visible := func(acl string, roles any, docsec, cr, cu, cd bool) bool {
		t.Helper()
		var got bool
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT public.tw_visible(?::text[], ?::text[], ?, ?, ?, ?)`,
			acl, roles, docsec, cr, cu, cd).Scan(&got))
		return got
	}

	// 可写即可读：update-only / delete-only ACE（无 read）仍可见。
	require.True(t, visible(`{update:user:b}`, `{user:b,any}`, true, false, false, false))
	require.True(t, visible(`{delete:user:b}`, `{user:b,any}`, true, false, false, false))
	// B14：key:<id> ACE 的可写即可读对 key 主体同构。
	require.True(t, visible(`{update:key:k1}`, `{key:k1,any}`, true, false, false, false))
	require.False(t, visible(`{read:key:k1}`, `{key:k2,any}`, true, true, true, true), "跨 key 不可见")
	// read-only 可见（写权由各自 policy 管，tw_visible 只管可见）。
	require.True(t, visible(`{read:user:b}`, `{user:b,any}`, true, false, false, false))
	// 无任何命中不可见（非空 ACE 覆盖集合级）。
	require.False(t, visible(`{read:user:a}`, `{user:b,any}`, true, true, true, true))
	// 空回退：集合级任一 op 授权即可见。
	require.True(t, visible(`{}`, `{any}`, true, false, true, false))
	// docSec=false：纯集合级（ACE 不参与）；集合级写权蕴含可见。
	require.False(t, visible(`{read:user:b}`, `{user:b,any}`, false, false, false, false))
	require.True(t, visible(`{read:user:b}`, `{user:a,any}`, false, false, true, false))
	// fail-closed。
	require.False(t, visible(`{}`, nil, true, true, true, true))
}

// ---------------------------------------------------------------------------
// 行为级断言（经 policy）
// ---------------------------------------------------------------------------

type rlsBehaviorEnv struct {
	docDB   databases.DocumentDB
	project string
}

func setupRLSBehaviorEnv(t *testing.T) *rlsBehaviorEnv {
	t.Helper()
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	t.Cleanup(cleanup)

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "read", Role: "any"},
	}, true))

	env := &rlsBehaviorEnv{docDB: docDB, project: projectID}
	mk := func(id string, perms []databases.Permission) {
		t.Helper()
		_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
			ID: id, Data: map[string]any{"title": id},
		}, perms, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	mk("private-a", []databases.Permission{{Type: "read", Role: "user:a"}})
	mk("update-only-b", []databases.Permission{{Type: "update", Role: "user:b"}})
	mk("write-c", []databases.Permission{{Type: "write", Role: "user:c"}})
	mk("read-only-d", []databases.Permission{{Type: "read", Role: "user:d"}})
	mk("empty-e", nil)
	// B14：per-key ACE 文档（keyA 种子形态，种子值 read/update/delete:key:ka）。
	mk("key-owned", []databases.Permission{
		{Type: "read", Role: "key:ka"},
		{Type: "update", Role: "key:ka"},
		{Type: "delete", Role: "key:ka"},
	})
	return env
}

// visibleIDs 以 principal 列出可见文档 ID 集。
func (e *rlsBehaviorEnv) visibleIDs(t *testing.T, principal databases.Principal) map[string]bool {
	t.Helper()
	ctx := context.Background()
	list, err := e.docDB.ListDocuments(ctx, e.project, "app", "docs", databases.Query{}, principal)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, d := range list.Documents {
		out[d.ID] = true
	}
	return out
}

func TestRLS_Behavior_VisibilityMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupRLSBehaviorEnv(t)
	ctx := context.Background()

	// guest（any）：仅空 ACE 文档经集合级 read:any 回退可见。
	guest := env.visibleIDs(t, databases.Principal{Roles: []string{"guests"}})
	require.Equal(t, map[string]bool{"empty-e": true}, guest)

	// user:a：private 可见 + 空回退；update-only/write/read-only 不可见。
	a := env.visibleIDs(t, databases.Principal{Roles: []string{"user:a"}})
	require.Equal(t, map[string]bool{"private-a": true, "empty-e": true}, a)

	// user:b（update-only）：可写即可读——可见且可改，不可删。
	b := env.visibleIDs(t, databases.Principal{Roles: []string{"user:b"}})
	require.Equal(t, map[string]bool{"update-only-b": true, "empty-e": true}, b)
	updated, err := env.docDB.UpdateDocument(ctx, env.project, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: "update-only-b", Data: map[string]any{"title": "b-touched"}},
		ExpectedVersion: 1,
	}, databases.Principal{Roles: []string{"user:b"}})
	require.NoError(t, err, "update-only 用户可见可改（可写即可读 + UPDATE policy）")
	require.Equal(t, "b-touched", updated.Data["title"])
	err = env.docDB.DeleteDocument(ctx, env.project, "app", "docs", "update-only-b",
		databases.DeleteOptions{ExpectedVersion: 2}, databases.Principal{Roles: []string{"user:b"}})
	require.ErrorIs(t, err, ErrPermissionDenied, "update-only 不可删（FOR UPDATE 可见但 DELETE policy 拒绝）")

	// user:c（write ACE）：create/update/delete 全命中。
	c := env.visibleIDs(t, databases.Principal{Roles: []string{"user:c"}})
	require.Equal(t, map[string]bool{"write-c": true, "empty-e": true}, c)
	require.NoError(t, env.docDB.DeleteDocument(ctx, env.project, "app", "docs", "write-c",
		databases.DeleteOptions{ExpectedVersion: 1}, databases.Principal{Roles: []string{"user:c"}}))

	// user:d（read-only）：可见不可改。
	d := env.visibleIDs(t, databases.Principal{Roles: []string{"user:d"}})
	require.Equal(t, map[string]bool{"read-only-d": true, "empty-e": true}, d)
	_, err = env.docDB.UpdateDocument(ctx, env.project, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: "read-only-d", Data: map[string]any{"title": "x"}},
		ExpectedVersion: 1,
	}, databases.Principal{Roles: []string{"user:d"}})
	require.ErrorIs(t, err, ErrPermissionDenied, "read-only 可见但不可改（探测：可见 + version 相符 ⇒ policy 拒绝）")

	// keys：无任何授予——仅空回退文档可见。
	keys := env.visibleIDs(t, databases.Principal{Roles: []string{"keys"}})
	require.Equal(t, map[string]bool{"empty-e": true}, keys)

	// B14 per-key：key:ka 持有者可见自身种子文档 + 空回退；key:kb 不可见
	// keyA 的文档（跨 key 隔离行为级锚点）。
	ka := env.visibleIDs(t, databases.Principal{Roles: []string{"keys", "key:ka"}})
	require.Equal(t, map[string]bool{"key-owned": true, "empty-e": true}, ka)
	kb := env.visibleIDs(t, databases.Principal{Roles: []string{"keys", "key:kb"}})
	require.Equal(t, map[string]bool{"empty-e": true}, kb)

	// SystemPrincipal 全路径旁路（BYPASSRLS）。
	sys := env.visibleIDs(t, databases.SystemPrincipal)
	require.Len(t, sys, 5, "BYPASSRLS 旁路：删除 write-c 后余 5 篇")
}

func TestRLS_Behavior_GetInvisibleIsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupRLSBehaviorEnv(t)
	ctx := context.Background()

	// 不可见 = 不存在（防枚举）：Get 返回 nil（app 层映射 NotFound）。
	got, err := env.docDB.GetDocument(ctx, env.project, "app", "docs", "private-a", databases.Principal{Roles: []string{"user:b"}})
	require.NoError(t, err)
	require.Nil(t, got)

	// 可见但不可删（DELETE policy 拒绝）→ PERMISSION_DENIED（非静默成功）。
	err = env.docDB.DeleteDocument(ctx, env.project, "app", "docs", "read-only-d",
		databases.DeleteOptions{ExpectedVersion: 1}, databases.Principal{Roles: []string{"user:d"}})
	require.ErrorIs(t, err, ErrPermissionDenied)
}

func TestRLS_Behavior_CreateDeniedByPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	defer cleanup()
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "locked", "Locked", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "user:a"},
	}, true))

	// user:b create 被拒：INSERT WITH CHECK → 42501 → PERMISSION_DENIED。
	_, err := docDB.CreateDocument(ctx, projectID, "app", "locked", databases.Document{
		Data: map[string]any{"title": "x"},
	}, nil, databases.Principal{Roles: []string{"user:b"}})
	require.ErrorIs(t, err, ErrPermissionDenied)

	_, err = docDB.CreateDocument(ctx, projectID, "app", "locked", databases.Document{
		Data: map[string]any{"title": "x"},
	}, nil, databases.Principal{Roles: []string{"user:a"}})
	require.NoError(t, err)
}

// captureACLPublisher 捕获 update 事件的写前 ACL 快照。
type captureACLPublisher struct {
	acl    []databases.Permission
	events []string
}

var _ shared.EventPublisher = (*captureACLPublisher)(nil)

func (c *captureACLPublisher) Publish(ctx context.Context, ev domainevents.Envelope) error {
	c.events = append(c.events, ev.Event)
	if ev.Event == domainevents.EventDocumentsUpdate {
		c.acl = ev.ACL.DocumentPermissions
	}
	return nil
}

// TestRLS_Behavior_SelfLock：把 _acl 改成排除自己（自锁）——UPDATE policy
// USING 按**旧值**裁决（语句启动时行可见），WITH CHECK 恒真放行新值（预决策 4
// "允许自锁"，C1 种子与事件写前快照依赖）。
func TestRLS_Behavior_SelfLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	defer cleanup()

	pub := &captureACLPublisher{}
	docDB := NewPostgresDocumentDB(db, pub)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
	}, true))

	alice := databases.Principal{Roles: []string{"users", "user:alice"}}
	_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID: "self-lock", Data: map[string]any{"title": "t"},
	}, []databases.Permission{
		{Type: "read", Role: "user:alice"},
		{Type: "update", Role: "user:alice"},
		{Type: "delete", Role: "user:alice"},
	}, alice)
	require.NoError(t, err)

	pub.acl = nil
	// 自锁：替换 _acl 只保留 bob。
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document: databases.Document{ID: "self-lock", Data: map[string]any{"title": "locked"}},
		Permissions: []databases.Permission{
			{Type: "read", Role: "user:bob"},
			{Type: "update", Role: "user:bob"},
			{Type: "delete", Role: "user:bob"},
		},
		ExpectedVersion: 1,
	}, alice)
	require.NoError(t, err, "自锁必须成功（USING 按旧值，WITH CHECK 恒真）")
	require.Equal(t, []databases.Permission{
		{Type: "read", Role: "user:bob"},
		{Type: "update", Role: "user:bob"},
		{Type: "delete", Role: "user:bob"},
	}, updated.Permissions, "读回（SystemPrincipal）应见新 _acl")
	// 事件快照 = 写前 ACE（alice 的三连）。
	require.Equal(t, []databases.Permission{
		{Type: "read", Role: "user:alice"},
		{Type: "update", Role: "user:alice"},
		{Type: "delete", Role: "user:alice"},
	}, pub.acl, "事件 acl 快照必须是写前 _acl")

	// 自锁后 alice 不再可见；bob 可见。
	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", "self-lock", alice)
	require.NoError(t, err)
	require.Nil(t, got)
	got, err = docDB.GetDocument(ctx, projectID, "app", "docs", "self-lock", databases.Principal{Roles: []string{"user:bob"}})
	require.NoError(t, err)
	require.NotNil(t, got)
}

// TestRLS_Behavior_TenantColumnLocked：列级 GRANT 排除 _tenant——tw_app 不可
// 写 _tenant（预决策 6）；SELECT 可读（WHERE 过滤需要）。
func TestRLS_Behavior_TenantColumnLocked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	defer cleanup()
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "any"},
	}, true))
	_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID: "t1", Data: map[string]any{"title": "x"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")
	tbl := testSchema(t, projectID, "app") + "." + physical

	// 手工 GUC 注入已不可见（roles_sig 验签，阶段③-b 包 C）：一律经
	// WithExecIdentity（InjectExecIdentity 自动签名）。
	err = db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{
		Role: clients.RoleApp, Roles: []string{"any"},
	}), func(txCtx context.Context) error {
		_, err := db.Conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`UPDATE %s SET _tenant = 999 WHERE _id = 't1'`, tbl))
		return err
	})
	require.Error(t, err, "_tenant 列必须对 tw_app 锁死（列级 GRANT 排除）")
	require.Contains(t, err.Error(), "42501")

	// SELECT _tenant 可读（查询谓词与载荷需要）。
	var tenant int64
	require.NoError(t, db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{
		Role: clients.RoleApp, Roles: []string{"any"},
	}), func(txCtx context.Context) error {
		return db.Conn(txCtx).QueryRowContext(txCtx,
			fmt.Sprintf(`SELECT _tenant FROM %s WHERE _id = 't1'`, tbl)).Scan(&tenant)
	}))
	require.NotZero(t, tenant)
}

// TestRLS_Behavior_ACLColumnLockedToSystemPath（R13a + 阶段③-b 包 C + R16 ③）：
// _acl 的**删改**通道唯一化为 tw_set_document_acl（000029 修订，函数内租户
// 绑定 + 可见性校验）。tw_app 即使注入正常 roles（policy 判定全放行）也不得
// 直改 _acl：UPDATE 列级 GRANT 排除 _acl（非自锁变更可过 SELECT policy 新行
// 复检，该旁路必须从列权限上封死）；INSERT 携带 _acl 是合法通道（R16 ③ 恢复：
// 新行无旧行、可见性校验不适用，create/upsert 插入支随行携带）。同步验证
// reconcile 矫正：手动模拟存量表的旧授权形态（GRANT UPDATE(_acl)），DDL touch
// 后被 REVOKE 重授形态矫正回锁死。
func TestRLS_Behavior_ACLColumnLockedToSystemPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	projectID, internalID, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	defer cleanup()
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "any"},
		{Type: "update", Role: "any"},
	}, true))
	alice := databases.Principal{Roles: []string{"users", "user:alice"}}
	_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID: "a1", Data: map[string]any{"title": "x"},
	}, []databases.Permission{{Type: "read", Role: "user:alice"}, {Type: "update", Role: "user:alice"}}, alice)
	require.NoError(t, err, "create 的 _acl 随 INSERT 携带（R16 ③ 恢复通道）")

	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")
	tbl := testSchema(t, projectID, "app") + "." + physical
	asApp := func(roles []string, tenant int64, fn func(txCtx context.Context) error) error {
		return db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{
			Role: clients.RoleApp, Roles: roles, Tenant: tenant,
		}), fn)
	}

	// tw_app 注入正常 roles（对该行 read+update 全放行）直改 _acl → 列权限拒绝。
	updateACLAsApp := func() error {
		return asApp([]string{"users", "user:alice", "any"}, internalID, func(txCtx context.Context) error {
			_, err := db.Conn(txCtx).ExecContext(txCtx,
				fmt.Sprintf(`UPDATE %s SET _acl = '{read:any}' WHERE _id = 'a1'`, tbl))
			return err
		})
	}
	err = updateACLAsApp()
	require.Error(t, err, "tw_app 直改 _acl 必须被列权限拒绝（R13a choke point）")
	require.Contains(t, err.Error(), "42501")
	// SQLSTATE 路径映射为 PERMISSION_DENIED（mapError 语义）。
	require.ErrorIs(t, docDB.mapError(err), ErrPermissionDenied)

	// R16 ③：INSERT 携带 _acl 是合法通道（create/upsert 插入支；集合级
	// create 判定由 INSERT WITH CHECK policy 承载）。
	require.NoError(t, asApp([]string{"users"}, internalID, func(txCtx context.Context) error {
		_, err := db.Conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
			`INSERT INTO %s (_id, _acl) VALUES ('seeded', '{read:any}')`, tbl))
		if err != nil {
			return err
		}
		_, err = db.Conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
			`DELETE FROM %s WHERE _id = 'seeded'`, tbl))
		return err
	}), "INSERT 携带 _acl 必须放行（R16 ③ 恢复）")

	// reconcile 矫正：模拟存量表旧授权形态（UPDATE 含 _acl），列授权全量
	// 扫描（门禁 A1 入口）后恢复锁死。（B3 语义收窄：CreateIndex 改在线
	// 通道后不再搭车 ensureCollectionRLS 自愈——授权矫正统一归 A1 扫描。）
	_, err = db.ExecContext(ctx, fmt.Sprintf(`GRANT UPDATE (_acl) ON %s TO tw_app`, tbl))
	require.NoError(t, err)
	require.NoError(t, updateACLAsApp(), "模拟旧授权形态下直改放行（对照组，确认矫正必要性）")
	// 对照组已把 _acl 改为 {read:any}（只读）——恢复造数，隔离后续自锁断言。
	_, err = db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET _acl = '{read:user:alice,update:user:alice}' WHERE _id = 'a1'`, tbl))
	require.NoError(t, err)
	// 列授权全量 reconcile（A1：refreshColumnGrants 幂等重建）。
	_, err = ReconcileCollectionColumnGrants(ctx, db)
	require.NoError(t, err)
	err = updateACLAsApp()
	require.Error(t, err, "A1 扫描后旧授权形态必须被矫正（REVOKE ALL → 按清单重授）")
	require.Contains(t, err.Error(), "42501")

	// 自锁路径回归：tw_app 经 updateDocument 替换 _acl（tw_set_document_acl
	// 函数通道，BYPASSRLS definer；R16 可见性校验对该行放行——alice 持
	// read:user:alice）仍正常。
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document: databases.Document{ID: "a1", Data: map[string]any{"title": "locked"}},
		Permissions: []databases.Permission{
			{Type: "read", Role: "user:bob"},
			{Type: "update", Role: "user:bob"},
		},
		ExpectedVersion: 1,
	}, alice)
	require.NoError(t, err, "自锁（tw_set_document_acl 路径）不受列级收紧影响")
	require.Equal(t, []databases.Permission{
		{Type: "read", Role: "user:bob"},
		{Type: "update", Role: "user:bob"},
	}, updated.Permissions)
}

// TestRLS_ExplainInitPlanGate（I1 最小版）：policy 的 catalog 子查询必须被
// InitPlan 化（每语句一次），EXPLAIN 计划含 InitPlan 且无逐行 SubPlan 于过滤层。
func TestRLS_ExplainInitPlanGate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil)
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	defer cleanup()
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "read", Role: "any"},
	}, true))
	_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID: "p1", Data: map[string]any{"title": "x"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")
	tbl := testSchema(t, projectID, "app") + "." + physical

	var plan strings.Builder
	// roles_sig（阶段③-b 包 C）：经 WithExecIdentity 注入（自动签名）。
	require.NoError(t, db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{
		Role: clients.RoleApp, Roles: []string{"any"},
	}), func(txCtx context.Context) error {
		rows, err := db.Conn(txCtx).QueryContext(txCtx,
			fmt.Sprintf(`EXPLAIN (COSTS OFF) SELECT _id FROM %s`, tbl))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		return rows.Err()
	}))
	require.Contains(t, plan.String(), "InitPlan", "catalog/roles 子查询必须 InitPlan 化（每语句一次，非逐行 SubPlan）")
}

// TestRLS_RelativeBenchmark（I1 最小版）：10 万行表 RLS 开（tw_app + policy）
// vs 关（tw_system BYPASSRLS）的 COUNT 相对基准——比值上限断言（绝对 P99 门禁
// 转 出 POC 后上 CI 机器基准）。数字进 t.Log 供复审报告引用。
func TestRLS_RelativeBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil)
	projectID, internalID, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	defer cleanup()
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "bench", "Bench", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 64},
	}, nil, []databases.Permission{
		{Type: "read", Role: "any"},
	}, true))
	physical := testPhysicalName(t, ctx, db, projectID, "app", "bench")
	tbl := testSchema(t, projectID, "app") + "." + physical

	const rows = 100000
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (_id, _tenant, title) SELECT 'b' || g, %d, 't' FROM generate_series(1, %d) g`,
		tbl, internalID, rows))
	require.NoError(t, err)

	countAs := func(role string, withRoles bool) time.Duration {
		t.Helper()
		var id clients.ExecIdentity
		switch role {
		case clients.RoleSystem:
			id = clients.ExecIdentity{Role: clients.RoleSystem}
		default:
			// roles_sig（阶段③-b 包 C）：手工 set_config 注入无签名不可见，
			// 一律经 WithExecIdentity（自动签名）。
			id = clients.ExecIdentity{Role: clients.RoleApp, Roles: []string{"any"}}
			if !withRoles {
				id.Roles = nil
			}
		}
		best := time.Duration(1 << 62)
		for i := 0; i < 3; i++ {
			require.NoError(t, db.RunInTx(clients.WithExecIdentity(ctx, id), func(txCtx context.Context) error {
				start := time.Now()
				var n int
				if err := db.Conn(txCtx).QueryRowContext(txCtx,
					fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&n); err != nil {
					return err
				}
				require.Equal(t, rows, n)
				if d := time.Since(start); d < best {
					best = d
				}
				return nil
			}))
		}
		return best
	}

	offDur := countAs(clients.RoleSystem, false)
	onDur := countAs(clients.RoleApp, true)
	ratio := float64(onDur) / float64(offDur)
	t.Logf("rls relative benchmark (rows=%d): rls_off=%v rls_on=%v ratio=%.1fx", rows, offDur, onDur, ratio)
	require.Less(t, ratio, 30.0, "RLS 开/关 COUNT 比值超上限（InitPlan 化失效或逐行 SubPlan 退化）")
}
