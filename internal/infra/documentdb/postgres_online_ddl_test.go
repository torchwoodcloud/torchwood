// 在线索引通道集成测试（转出 POC 门禁 B3，docs/developer/15-exit-poc.md）：
//   - 大表建索引期间并发读写不被阻塞（持锁注入用例——判据原文）；
//   - building→active 状态机有中断恢复用例（building 残留可重入——判据原文）；
//   - failed 路径（unique 撞存量重复）清理 INVALID 残留且可重入。
//
// CONCURRENTLY 需独立连接，持锁注入经池外 *sql.Conn 直开会话（B3 判据的
// 测试形态约定）；崩溃残留经工作树内直接 UPDATE catalog 模拟。
package documentdb

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

type onlineIndexEnv struct {
	db         *clients.Database
	p          *postgresDocumentDB
	project    string
	schema     string
	physical   string
	collection string
	database   string
	docIDs     []string
}

func setupOnlineIndexEnv(t *testing.T, docs int) *onlineIndexEnv {
	t.Helper()
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	p := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	t.Cleanup(cleanup)
	require.NoError(t, p.CreateDatabase(ctx, projectID, "app", "App DB"))
	attrs := []databases.Attribute{
		{ID: "code", Key: "code", Type: "string", Size: 64},
		{ID: "qty", Key: "qty", Type: "integer"},
	}
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "docs", "Docs", attrs, nil, anyPerms(), true))
	env := &onlineIndexEnv{
		db: db, p: p, project: projectID,
		schema: testSchema(t, projectID, "app"), database: "app", collection: "docs",
		physical: testPhysicalName(t, ctx, db, projectID, "app", "docs"),
	}
	for i := 0; i < docs; i++ {
		doc, err := p.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
			Data: map[string]any{"code": "c" + strconv.Itoa(i), "qty": int64(i + 1)},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
		env.docIDs = append(env.docIDs, doc.ID)
	}
	return env
}

// catalogIndexStatus 读 catalog 中指定索引条目的 status（崩溃残留模拟与状态
// 断言的直查通道）；条目缺失返回 "absent"。
func (env *onlineIndexEnv) catalogIndexStatus(t *testing.T, ctx context.Context, indexID string) string {
	t.Helper()
	m := new(model.DocumentCollection)
	require.NoError(t, env.db.NewSelect().Model(m).
		Where("project_id = ? AND database_id = ? AND collection_id = ?", env.project, env.database, env.collection).
		Scan(ctx))
	idxs, err := decodeIndexes(m.Indexes)
	require.NoError(t, err)
	for _, i := range idxs {
		if i.ID == indexID {
			return i.StatusOrDefault()
		}
	}
	return "absent"
}

// markIndexStatusDirect 绕过状态机直改 catalog（模拟进程崩溃残留——B3 判据
// 的测试形态约定：工作树内直接 UPDATE catalog；age 把 updated_at 推老以越过
// 超时阈值）。
func (env *onlineIndexEnv) markIndexStatusDirect(t *testing.T, ctx context.Context, indexID, status string, age time.Duration) {
	t.Helper()
	m := new(model.DocumentCollection)
	require.NoError(t, env.db.NewSelect().Model(m).
		Where("project_id = ? AND database_id = ? AND collection_id = ?", env.project, env.database, env.collection).
		Scan(ctx))
	idxs, err := decodeIndexes(m.Indexes)
	require.NoError(t, err)
	found := false
	for i := range idxs {
		if idxs[i].ID == indexID {
			idxs[i].Status = status
			found = true
		}
	}
	require.True(t, found, "index %s not in catalog", indexID)
	idxsJSON, err := encodeIndexes(idxs)
	require.NoError(t, err)
	m.Indexes = idxsJSON
	m.UpdatedAt = time.Now().Add(-age)
	_, err = env.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	require.NoError(t, err)
}

// physicalIndexExists 查物理索引是否存在（pg_class 直查，不区分 valid）。
func (env *onlineIndexEnv) physicalIndexExists(t *testing.T, ctx context.Context, indexID string) bool {
	t.Helper()
	var n int
	require.NoError(t, env.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = ? AND c.relname = ?`, env.schema, physicalIndexName(env.physical, indexID)).Scan(&n))
	return n > 0
}

// physicalIndexValid 查物理索引的 indisvalid（缺失返回 false）。
func (env *onlineIndexEnv) physicalIndexValid(t *testing.T, ctx context.Context, indexID string) bool {
	t.Helper()
	var valid bool
	err := env.db.QueryRowContext(ctx,
		`SELECT i.indisvalid FROM pg_index i
		 JOIN pg_class c ON c.oid = i.indexrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = ? AND c.relname = ?`, env.schema, physicalIndexName(env.physical, indexID)).Scan(&valid)
	if err != nil {
		return false
	}
	return valid
}

// dropPhysicalIndexDirect 直删物理索引（模拟崩溃时事务 B 未及执行等残留态）。
func (env *onlineIndexEnv) dropPhysicalIndexDirect(t *testing.T, ctx context.Context, indexID string) {
	t.Helper()
	_, err := env.db.ExecContext(ctx, dropIndexStatement(env.schema, physicalIndexName(env.physical, indexID)))
	require.NoError(t, err)
}

// TestOnlineIndex_TwoPhaseHappyPath：在线通道落位——catalog status=active、
// 物理索引 valid、重复创建 AlreadyExists。
func TestOnlineIndex_TwoPhaseHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupOnlineIndexEnv(t, 10)

	require.NoError(t, env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "by_code", Type: "key", Attributes: []string{"code"},
	}))
	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "by_code"))
	require.True(t, env.physicalIndexExists(t, ctx, "by_code"))
	require.True(t, env.physicalIndexValid(t, ctx, "by_code"))

	err := env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "by_code", Type: "key", Attributes: []string{"code"},
	})
	require.ErrorIs(t, err, ErrDuplicateKey)
}

// TestOnlineIndex_ConcurrentDMLNotBlocked 是门禁判据本体（持锁注入）：CIC
// 构建挂起期间（pg_index 行已建、indisvalid=false）并发读写不被阻塞。
// 对照组：同持锁场景下非并发 CREATE INDEX（SHARE 锁）被写事务阻塞——
// lock_timeout 触发，证明注入真实生效、两通道行为差异真实存在。
func TestOnlineIndex_ConcurrentDMLNotBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupOnlineIndexEnv(t, 50)
	tbl := env.schema + "." + env.physical

	// 持锁注入：池外独立连接开写事务（ROW EXCLUSIVE，等价"大表上有活跃长
	// 写事务"的生产场景）。
	holder, err := env.db.DB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = holder.ExecContext(context.Background(), "ROLLBACK")
		_, _ = holder.ExecContext(context.Background(), "RESET ROLE")
		_ = holder.Close()
	})
	_, err = holder.ExecContext(ctx, "BEGIN")
	require.NoError(t, err)
	_, err = holder.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (_id, code, qty) VALUES ('lock-holder', 'held', 1)`, tbl))
	require.NoError(t, err)

	// 对照组：非并发 CREATE INDEX 需 SHARE 锁，被持锁写事务阻塞 →
	// lock_timeout 触发（证明注入真实生效）。
	blocker, err := env.db.DB.Conn(ctx)
	require.NoError(t, err)
	_, err = blocker.ExecContext(ctx, `SET ROLE tw_owner; SET lock_timeout TO '400ms'`)
	require.NoError(t, err)
	_, err = blocker.ExecContext(ctx, `CREATE INDEX idx_`+env.physical+`_blocked ON `+tbl+` (qty)`)
	require.ErrorContains(t, err, "lock timeout", "注入未生效：非并发建索引应被写事务阻塞")
	_, _ = blocker.ExecContext(context.Background(), "RESET ROLE")
	_ = blocker.Close()

	// 门禁本体：在线通道在同一持锁场景下启动——CIC 首事务提交索引行
	//（indisvalid=false）后进入等待；断言其挂起期间并发读写不被阻塞。
	done := make(chan error, 1)
	go func() {
		done <- env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
			ID: "by_qty", Type: "key", Attributes: []string{"qty"},
		})
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if env.physicalIndexExists(t, ctx, "by_qty") && !env.physicalIndexValid(t, ctx, "by_qty") {
			break
		}
		select {
		case err := <-done:
			require.NoError(t, err, "CIC goroutine exited before reaching pending state")
			t.Fatal("CIC completed without being observed pending")
		case <-time.After(100 * time.Millisecond):
		}
	}
	require.True(t, env.physicalIndexExists(t, ctx, "by_qty") && !env.physicalIndexValid(t, ctx, "by_qty"),
		"CIC never reached pending (indisvalid=false) state")
	select {
	case err := <-done:
		require.NoError(t, err)
		t.Fatal("CIC should still be pending behind the open holder transaction")
	case <-time.After(200 * time.Millisecond):
		// 仍在构建（预期）：进入"并发读写不阻塞"断言。
	}

	// 构建挂起期间：写（新文档）与读全部即时成功——非并发通道在此场景必然
	// 阻塞（对照组已证）。
	_, err = env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
		Data: map[string]any{"code": "during", "qty": int64(999)},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err, "CIC 挂起期间写被阻塞")
	docs, err := env.p.ListDocuments(ctx, env.project, env.database, env.collection,
		databases.Query{PageSize: 10}, databases.SystemPrincipal)
	require.NoError(t, err, "CIC 挂起期间读被阻塞")
	require.NotEmpty(t, docs.Documents)

	// 释放持锁事务：CIC 完成等待 → 事务 B 置 active。
	_, err = holder.ExecContext(ctx, "ROLLBACK")
	require.NoError(t, err)
	require.NoError(t, <-done)
	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "by_qty"))
	require.True(t, env.physicalIndexValid(t, ctx, "by_qty"))
}

// TestOnlineIndex_BuildingResidualReentry 是门禁判据本体（中断恢复）：
// 进程崩溃留 building 残留（工作树内直改 catalog 模拟）→ 同 ID CreateIndex
// 可重入 → 收敛 active；物理索引缺失（崩溃在 CIC 前）与存在且 valid（崩溃在
// 事务 B 前）两个中断点都收敛。
func TestOnlineIndex_BuildingResidualReentry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupOnlineIndexEnv(t, 10)

	// 中断点 1：事务 A 已提交（building 残留），CIC 未及执行（物理索引缺失）。
	require.NoError(t, env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "midway", Type: "key", Attributes: []string{"code"},
	}))
	env.dropPhysicalIndexDirect(t, ctx, "midway")
	env.markIndexStatusDirect(t, ctx, "midway", databases.IndexStatusBuilding, 2*time.Hour)
	require.False(t, env.physicalIndexExists(t, ctx, "midway"))

	require.NoError(t, env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "midway", Type: "key", Attributes: []string{"code"},
	}), "building 残留必须可重入")
	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "midway"))
	require.True(t, env.physicalIndexValid(t, ctx, "midway"))

	// 中断点 2：CIC 已完成（物理索引 valid），事务 B 未及执行。
	env.markIndexStatusDirect(t, ctx, "midway", databases.IndexStatusBuilding, 2*time.Hour)
	require.True(t, env.physicalIndexValid(t, ctx, "midway"))
	require.NoError(t, env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "midway", Type: "key", Attributes: []string{"code"},
	}))
	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "midway"))
	require.True(t, env.physicalIndexValid(t, ctx, "midway"))
}

// TestOnlineIndex_FailedPathAndReentry：unique 索引撞存量重复 → CIC 失败 →
// INVALID 残留被清理、catalog 置 failed；重复值消除后重入收敛 active。
func TestOnlineIndex_FailedPathAndReentry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupOnlineIndexEnv(t, 10)
	// 同 code 重复（unique 不可满足）。
	for _, id := range []string{"dup-1", "dup-2"} {
		_, err := env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
			ID: id, Data: map[string]any{"code": "dup", "qty": int64(1)},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}

	err := env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "uniq_code", Type: "unique", Attributes: []string{"code"},
	})
	require.Error(t, err, "unique 撞存量重复必须失败")
	require.Equal(t, databases.IndexStatusFailed, env.catalogIndexStatus(t, ctx, "uniq_code"))
	require.False(t, env.physicalIndexExists(t, ctx, "uniq_code"),
		"失败的 CIC 残留 INVALID 索引必须被清理")

	// 消除重复 → 重入（failed → building → active）。
	for _, id := range []string{"dup-1", "dup-2"} {
		require.NoError(t, env.p.DeleteDocument(ctx, env.project, env.database, env.collection, id,
			databases.DeleteOptions{SkipVersion: true}, databases.SystemPrincipal))
	}
	require.NoError(t, env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "uniq_code", Type: "unique", Attributes: []string{"code"},
	}), "failed 残留必须可重入")
	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "uniq_code"))
	require.True(t, env.physicalIndexValid(t, ctx, "uniq_code"))
}

// TestOnlineIndex_BuildStatementSingleSource：CONCURRENTLY 通道与事务内通道
// 的 DDL 表达式逐字同源（仅差 CONCURRENTLY 关键字）——形态漂移防线。
func TestOnlineIndex_BuildStatementSingleSource(t *testing.T) {
	attrs := []databases.Attribute{
		{ID: "code", Key: "code", Type: "string", Size: 64},
		{ID: "tags", Key: "tags", Type: "string", Array: true},
		{ID: "emb", Key: "emb", Type: "vector", Dims: 3},
	}
	cases := []databases.Index{
		{ID: "a", Type: "key", Attributes: []string{"code"}, Orders: []string{"desc"}},
		{ID: "b", Type: "unique", Attributes: []string{"code"}},
		{ID: "c", Type: "fulltext", Attributes: []string{"code"}},
		{ID: "d", Type: "key", Attributes: []string{"tags"}},
		{ID: "e", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "COSINE"},
	}
	for _, idx := range cases {
		plain, err := buildIndexStatement("s", "c_x", idx, attrs, false)
		require.NoError(t, err, "%s", idx.ID)
		conc, err := buildIndexStatement("s", "c_x", idx, attrs, true)
		require.NoError(t, err, "%s", idx.ID)
		require.Contains(t, conc, " CONCURRENTLY ", "%s: %s vs %s", idx.ID, plain, conc)
		require.NotContains(t, plain, "CONCURRENTLY")
		// 逐字同源：去掉 CONCURRENTLY 后两串完全一致。
		require.Equal(t, plain, strings.Replace(conc, " CONCURRENTLY", "", 1), "%s", idx.ID)
	}
}
