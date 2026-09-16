package bunrepo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// 本文件覆盖 docs/design/runtime-vars.md §6 repo 集成段：五类型值 roundtrip、
// 集合 CRUD + epoch、var CRUD + Update 白名单、可见性过滤、LockHead 契约、
// 版本链、回滚 roundtrip、版本淘汰窗口、并发写、footprint、项目删除 CASCADE。

// nowMicro 返回截断到微秒的 UTC 时刻（PG TIMESTAMPTZ 精度，往返无损）。
func nowMicro() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// newEpoch 按设计口径生成 8 字节随机 hex（use-case 层职责，测试内模拟）。
func newEpoch(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

func newVarSet(t *testing.T, projectID, varSetID, visibility string) projects.VarSet {
	t.Helper()
	now := nowMicro()
	return projects.VarSet{
		ProjectID:   projectID,
		VarSetID:    varSetID,
		Visibility:  visibility,
		Epoch:       newEpoch(t),
		Description: "",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func newRuntimeVar(projectID, varSetID, key, valueType, value, description string) projects.RuntimeVar {
	now := nowMicro()
	return projects.RuntimeVar{
		ProjectID:   projectID,
		VarSetID:    varSetID,
		Key:         key,
		ValueType:   valueType,
		Value:       value,
		Description: description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// runtimeVarSnapshotEntry 是版本快照 {key: {t, v, d, c}} 的单键结构（D11
// 全列四元组）。序列化格式属 app 层职责，此处与 repo 测试共用模拟实现。
type runtimeVarSnapshotEntry struct {
	T string `json:"t"`
	V string `json:"v"`
	D string `json:"d"`
	C string `json:"c"`
}

func runtimeVarSnapshotJSON(vars []projects.RuntimeVar) (string, error) {
	m := make(map[string]runtimeVarSnapshotEntry, len(vars))
	for _, v := range vars {
		m[v.Key] = runtimeVarSnapshotEntry{
			T: v.ValueType,
			V: v.Value,
			D: v.Description,
			C: v.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func runtimeVarSnapshotVars(snap string) ([]projects.RuntimeVar, error) {
	var m map[string]runtimeVarSnapshotEntry
	if err := json.Unmarshal([]byte(snap), &m); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]projects.RuntimeVar, 0, len(keys))
	for _, k := range keys {
		e := m[k]
		c, err := time.Parse(time.RFC3339Nano, e.C)
		if err != nil {
			return nil, err
		}
		out = append(out, projects.RuntimeVar{
			Key:         k,
			ValueType:   e.T,
			Value:       e.V,
			Description: e.D,
			CreatedAt:   c,
		})
	}
	return out, nil
}

// runtimeVarWriteFlow 模拟 app 用例层的单事务写路径（§2.5）：锁 heads →
// 变更 vars 行 → 写后 DB 回读构造快照 → INSERT 版本行（revision=锁定值+1）
// → bump+淘汰（淘汰窗口计入本次新版本行）。
func runtimeVarWriteFlow(ctx context.Context, db *clients.Database, repo projects.RuntimeVarRepository,
	projectID, varSetID, action, summary, actor string, mutate func(ctx context.Context) error,
) error {
	return db.RunInTx(ctx, func(ctx context.Context) error {
		locked, found, err := repo.LockHead(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("var set %s not found", varSetID)
		}
		if err := mutate(ctx); err != nil {
			return err
		}
		vars, err := repo.ListRuntimeVars(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		snap, err := runtimeVarSnapshotJSON(vars)
		if err != nil {
			return err
		}
		if err := repo.InsertVersion(ctx, &projects.RuntimeVarVersion{
			ProjectID: projectID,
			VarSetID:  varSetID,
			Revision:  locked + 1,
			Vars:      snap,
			Action:    action,
			Summary:   summary,
			Actor:     actor,
			CreatedAt: nowMicro(),
		}); err != nil {
			return err
		}
		newRev, err := repo.BumpRevisionAndPrune(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		if newRev != locked+1 {
			return fmt.Errorf("bump revision = %d, want %d", newRev, locked+1)
		}
		return nil
	})
}

// runtimeVarRollbackFlow 模拟 app 用例层的回滚路径（§2.5）：锁 heads → 读
// 目标版本快照 → 整替 → INSERT rollback 版本行（快照原样）→ bump+淘汰。
func runtimeVarRollbackFlow(ctx context.Context, db *clients.Database, repo projects.RuntimeVarRepository,
	projectID, varSetID string, targetRevision int64, actor string,
) error {
	return db.RunInTx(ctx, func(ctx context.Context) error {
		locked, found, err := repo.LockHead(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("var set %s not found", varSetID)
		}
		ver, err := repo.GetVersion(ctx, projectID, varSetID, targetRevision)
		if err != nil {
			return err
		}
		if ver == nil {
			return fmt.Errorf("target revision %d pruned", targetRevision)
		}
		vars, err := runtimeVarSnapshotVars(ver.Vars)
		if err != nil {
			return err
		}
		if err := repo.ReplaceVars(ctx, projectID, varSetID, vars); err != nil {
			return err
		}
		if err := repo.InsertVersion(ctx, &projects.RuntimeVarVersion{
			ProjectID: projectID,
			VarSetID:  varSetID,
			Revision:  locked + 1,
			Vars:      ver.Vars, // 目标快照原样
			Action:    projects.RuntimeVarActionRollback,
			Summary:   fmt.Sprintf("rollback to %d", targetRevision),
			Actor:     actor,
			CreatedAt: nowMicro(),
		}); err != nil {
			return err
		}
		newRev, err := repo.BumpRevisionAndPrune(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		if newRev != locked+1 {
			return fmt.Errorf("bump revision = %d, want %d", newRev, locked+1)
		}
		return nil
	})
}

// runtimeVarTableCount 统计某 runtime 表在项目下的行数（表名来自代码内
// 白名单枚举，非外部输入）。
func runtimeVarTableCount(ctx context.Context, t *testing.T, db *clients.Database, table, projectID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE project_id = ?", projectID).Scan(&n))
	return n
}

// requireSnapshotEqual 断言两份快照 JSON 文本语义相等（JSONB 归一化会重排
// 键序/空白，不能按字节比）。
func requireSnapshotEqual(t *testing.T, got, want string) {
	t.Helper()
	var gm, wm map[string]runtimeVarSnapshotEntry
	require.NoError(t, json.Unmarshal([]byte(got), &gm))
	require.NoError(t, json.Unmarshal([]byte(want), &wm))
	require.Equal(t, wm, gm)
}

// --- 五类型值 roundtrip（D3：JSONB 单列 + value_type 锚点） ---

func TestRuntimeVarRepo_ValueRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))

	cases := []struct {
		key       string
		valueType string
		value     string // 标准化 JSON 文本（标量即 JSON 标量）
	}{
		{"str_val", projects.RuntimeVarTypeString, `"hello world"`},
		{"int_val", projects.RuntimeVarTypeInteger, `42`},
		{"int_max", projects.RuntimeVarTypeInteger, `9007199254740991`}, // ±2^53-1 写入边界
		{"int_neg", projects.RuntimeVarTypeInteger, `-42`},
		{"float_val", projects.RuntimeVarTypeFloat, `3.14`},
		{"bool_val", projects.RuntimeVarTypeBoolean, `true`},
		{"json_val", projects.RuntimeVarTypeJSON, `{"a": 1, "b": [true, null]}`},
	}
	for _, c := range cases {
		v := newRuntimeVar(projectID, "config", c.key, c.valueType, c.value, "desc of "+c.key)
		require.NoErrorf(t, repo.CreateRuntimeVar(ctx, &v), "create %s", c.key)
	}
	for _, c := range cases {
		got, err := repo.GetRuntimeVar(ctx, projectID, "config", c.key)
		require.NoErrorf(t, err, "get %s", c.key)
		require.NotNilf(t, got, "get %s", c.key)
		require.Equal(t, c.valueType, got.ValueType, "value_type roundtrip: %s", c.key)
		if c.valueType == projects.RuntimeVarTypeJSON {
			// JSONB 归一化重排键序/空白：语义相等即可。
			var gm, wm any
			require.NoError(t, json.Unmarshal([]byte(got.Value), &gm))
			require.NoError(t, json.Unmarshal([]byte(c.value), &wm))
			require.Equal(t, wm, gm, "jsonb roundtrip fidelity: %s", c.key)
		} else {
			require.Equal(t, c.value, got.Value, "scalar jsonb roundtrip fidelity: %s", c.key)
		}
	}

	all, err := repo.ListRuntimeVars(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, all, len(cases))
}

// --- 集合 CRUD + epoch（D2：同名删除重建 epoch 必变） ---

func TestRuntimeVarRepo_VarSetCRUDAndEpoch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)

	// 创建 + epoch roundtrip。
	vs := newVarSet(t, projectID, "flags", projects.VarSetVisibilityPublic)
	vs.Description = "feature flags"
	require.NoError(t, repo.CreateVarSet(ctx, &vs))
	got, err := repo.GetVarSet(ctx, projectID, "flags")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, vs.Epoch, got.Epoch, "epoch roundtrip")
	require.Equal(t, projects.VarSetVisibilityPublic, got.Visibility)
	require.Equal(t, "feature flags", got.Description)
	require.True(t, got.CreatedAt.Equal(vs.CreatedAt))

	// heads 行同事务建立：GetHead 立即可见，revision 0。
	rev, found, err := repo.GetHead(ctx, projectID, "flags")
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, rev)

	// 主键冲突（app 层映射 409）。
	dup := newVarSet(t, projectID, "flags", projects.VarSetVisibilityPublic)
	require.Error(t, repo.CreateVarSet(ctx, &dup), "重复 (project_id, var_set_id) 必须拒绝")

	// 跨项目 miss。
	cross, err := repo.GetVarSet(ctx, "other_project", "flags")
	require.NoError(t, err)
	require.Nil(t, cross)

	// 列表：created_at DESC + var_set_id 决胜稳定排序。
	vs2 := newVarSet(t, projectID, "alpha", projects.VarSetVisibilityPrivate)
	vs2.CreatedAt = vs.CreatedAt.Add(time.Second)
	vs2.UpdatedAt = vs2.CreatedAt
	require.NoError(t, repo.CreateVarSet(ctx, &vs2))
	sets, err := repo.ListVarSets(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, sets, 2)
	require.Equal(t, "alpha", sets[0].VarSetID, "created_at DESC")
	require.Equal(t, "flags", sets[1].VarSetID)

	// Update 白名单：合法列可改，越权列拒绝。
	require.NoError(t, repo.UpdateVarSet(ctx, projectID, "flags", map[string]any{
		"visibility": projects.VarSetVisibilityPrivate,
		"updated_at": nowMicro(),
	}))
	got, err = repo.GetVarSet(ctx, projectID, "flags")
	require.NoError(t, err)
	require.Equal(t, projects.VarSetVisibilityPrivate, got.Visibility)
	require.Error(t, repo.UpdateVarSet(ctx, projectID, "flags", map[string]any{"epoch": "deadbeef"}))
	require.Error(t, repo.UpdateVarSet(ctx, projectID, "flags", map[string]any{"var_set_id": "renamed"}))
	// 元数据变更不动 heads（D10 在版本链测试中另有断言）。
	rev, found, err = repo.GetHead(ctx, projectID, "flags")
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, rev)

	// 删除 → 集合/heads 均不可见。
	require.NoError(t, repo.DeleteVarSet(ctx, projectID, "flags"))
	got, err = repo.GetVarSet(ctx, projectID, "flags")
	require.NoError(t, err)
	require.Nil(t, got)
	_, found, err = repo.GetHead(ctx, projectID, "flags")
	require.NoError(t, err)
	require.False(t, found, "heads 行随集合 CASCADE 消失")

	// 同名重建：新 epoch（D2 防碰撞），revision 归零。
	vs3 := newVarSet(t, projectID, "flags", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs3))
	require.NotEqual(t, vs.Epoch, vs3.Epoch, "重建集合 epoch 必变（随机生成）")
	rev, found, err = repo.GetHead(ctx, projectID, "flags")
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, rev, "重建后 revision 归零")
}

// --- var CRUD + Update 白名单 ---

func TestRuntimeVarRepo_VarCRUDAndUpdateWhitelist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))

	// create + get。
	v := newRuntimeVar(projectID, "config", "max_retry", projects.RuntimeVarTypeInteger, "3", "重试上限")
	require.NoError(t, repo.CreateRuntimeVar(ctx, &v))
	got, err := repo.GetRuntimeVar(ctx, projectID, "config", "max_retry")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "3", got.Value)
	require.Equal(t, "重试上限", got.Description)
	require.True(t, got.CreatedAt.Equal(v.CreatedAt))

	// 重复 key（app 层映射 409）。
	dup := newRuntimeVar(projectID, "config", "max_retry", projects.RuntimeVarTypeInteger, "5", "")
	require.Error(t, repo.CreateRuntimeVar(ctx, &dup))

	// 不存在 miss。
	missing, err := repo.GetRuntimeVar(ctx, projectID, "config", "nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	// Update 白名单：value/value_type/description/updated_at；key/created_at 越权拒绝。
	newUpdatedAt := nowMicro()
	require.NoError(t, repo.UpdateRuntimeVar(ctx, projectID, "config", "max_retry", map[string]any{
		"value":       `"5"`,
		"value_type":  projects.RuntimeVarTypeString,
		"description": "改为字符串",
		"updated_at":  newUpdatedAt,
	}))
	got, err = repo.GetRuntimeVar(ctx, projectID, "config", "max_retry")
	require.NoError(t, err)
	require.Equal(t, `"5"`, got.Value, "类型可随 Update 显式变更")
	require.Equal(t, projects.RuntimeVarTypeString, got.ValueType)
	require.Equal(t, "改为字符串", got.Description)
	require.True(t, got.UpdatedAt.Equal(newUpdatedAt))
	require.True(t, got.CreatedAt.Equal(v.CreatedAt), "created_at 不随 Update 变")
	require.Error(t, repo.UpdateRuntimeVar(ctx, projectID, "config", "max_retry", map[string]any{"key": "renamed"}))
	require.Error(t, repo.UpdateRuntimeVar(ctx, projectID, "config", "max_retry", map[string]any{"created_at": nowMicro()}))

	// list（key ASC）+ delete。
	v2 := newRuntimeVar(projectID, "config", "alpha", projects.RuntimeVarTypeBoolean, "false", "")
	require.NoError(t, repo.CreateRuntimeVar(ctx, &v2))
	listed, err := repo.ListRuntimeVars(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, "alpha", listed[0].Key)
	require.Equal(t, "max_retry", listed[1].Key)

	require.NoError(t, repo.DeleteRuntimeVar(ctx, projectID, "config", "max_retry"))
	got, err = repo.GetRuntimeVar(ctx, projectID, "config", "max_retry")
	require.NoError(t, err)
	require.Nil(t, got)
}

// --- RuntimeVarPublicRead 可见性（§2.4：private 拒绝做进 repo 实现） ---

func TestRuntimeVarRepo_PublicReadVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	pub := bunrepo.NewRuntimeVarPublicRead(db)

	pubSet := newVarSet(t, projectID, "public_set", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &pubSet))
	a := newRuntimeVar(projectID, "public_set", "a", projects.RuntimeVarTypeString, `"va"`, "")
	require.NoError(t, repo.CreateRuntimeVar(ctx, &a))

	privSet := newVarSet(t, projectID, "priv_set", projects.VarSetVisibilityPrivate)
	require.NoError(t, repo.CreateVarSet(ctx, &privSet))
	b := newRuntimeVar(projectID, "priv_set", "b", projects.RuntimeVarTypeString, `"vb"`, "")
	require.NoError(t, repo.CreateRuntimeVar(ctx, &b))

	// public 集：匿名（principalAllowed=false）恒可见。
	vars, epoch, revision, err := pub.GetVisibleVars(ctx, projectID, "public_set", false)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, "a", vars[0].Key)
	require.Equal(t, `"va"`, vars[0].Value)
	require.Equal(t, pubSet.Epoch, epoch)
	require.Zero(t, revision)

	// public 集 + 登录用户：同样可见。
	_, _, _, err = pub.GetVisibleVars(ctx, projectID, "public_set", true)
	require.NoError(t, err)

	// private 集 + allowed：可见。
	vars, epoch, revision, err = pub.GetVisibleVars(ctx, projectID, "priv_set", true)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, "b", vars[0].Key)
	require.Equal(t, privSet.Epoch, epoch)
	require.Zero(t, revision)

	// private 集 + 匿名：(nil, "", 0, nil)——与"集合不存在"不可区分。
	vars, epoch, revision, err = pub.GetVisibleVars(ctx, projectID, "priv_set", false)
	require.NoError(t, err)
	require.Nil(t, vars)
	require.Empty(t, epoch)
	require.Zero(t, revision)

	// 不存在集合（allowed 两种取值）：与上完全同形。
	for _, allowed := range []bool{false, true} {
		vars, epoch, revision, err = pub.GetVisibleVars(ctx, projectID, "ghost_set", allowed)
		require.NoError(t, err)
		require.Nil(t, vars)
		require.Empty(t, epoch)
		require.Zero(t, revision)
	}

	// 可见但空集：空切片（非 nil）+ epoch + revision。
	emptySet := newVarSet(t, projectID, "empty_set", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &emptySet))
	vars, epoch, revision, err = pub.GetVisibleVars(ctx, projectID, "empty_set", false)
	require.NoError(t, err)
	require.NotNil(t, vars, "可见空集返回空切片而非 nil（与不可见区分）")
	require.Empty(t, vars)
	require.Equal(t, emptySet.Epoch, epoch)
	require.Zero(t, revision)

	// 可见性切换是数据级属性：private→public 后匿名立即可见（走同一过滤路径）。
	require.NoError(t, repo.UpdateVarSet(ctx, projectID, "priv_set", map[string]any{
		"visibility": projects.VarSetVisibilityPublic,
		"updated_at": nowMicro(),
	}))
	vars, _, _, err = pub.GetVisibleVars(ctx, projectID, "priv_set", false)
	require.NoError(t, err)
	require.Len(t, vars, 1)
}

// --- LockHead 契约（0 行 = 集合不存在 → app 层 NotFound） ---

func TestRuntimeVarRepo_LockHead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)

	// 集合不存在（含 DeleteVarSet 竞态后的形态）：found=false 而非错误。
	rev, found, err := repo.LockHead(ctx, projectID, "ghost")
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, rev)

	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))

	// 事务内锁行 + bump 后可再锁读新值。
	err = db.RunInTx(ctx, func(ctx context.Context) error {
		rev, found, err := repo.LockHead(ctx, projectID, "config")
		if err != nil || !found {
			return fmt.Errorf("lock head: found=%v err=%v", found, err)
		}
		if rev != 0 {
			return fmt.Errorf("initial revision = %d, want 0", rev)
		}
		newRev, err := repo.BumpRevisionAndPrune(ctx, projectID, "config")
		if err != nil {
			return err
		}
		if newRev != 1 {
			return fmt.Errorf("first bump = %d, want 1", newRev)
		}
		return nil
	})
	require.NoError(t, err)

	gotRev, found, err := repo.GetHead(ctx, projectID, "config")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 1, gotRev)

	// 锁随事务提交释放：新事务可再锁。
	err = db.RunInTx(ctx, func(ctx context.Context) error {
		_, found, err := repo.LockHead(ctx, projectID, "config")
		if err != nil || !found {
			return fmt.Errorf("re-lock after commit: found=%v err=%v", found, err)
		}
		return nil
	})
	require.NoError(t, err)

	// 删除集合后锁 0 行（DeleteVarSet 竞态下写事务据此报 NotFound）。
	require.NoError(t, repo.DeleteVarSet(ctx, projectID, "config"))
	rev, found, err = repo.LockHead(ctx, projectID, "config")
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, rev)
}

// --- 版本链（每次写恰好一版、revision 严格单调、元数据变更不入链） ---

func TestRuntimeVarRepo_VersionChain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))

	actor := "admin:admin1"
	aCreate := newRuntimeVar(projectID, "config", "a", projects.RuntimeVarTypeInteger, "1", "d1")
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionCreate, "create: a", actor,
		func(ctx context.Context) error { return repo.CreateRuntimeVar(ctx, &aCreate) }))
	aUpdate := map[string]any{"value": "2", "updated_at": nowMicro()}
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionUpdate, "update: a", actor,
		func(ctx context.Context) error { return repo.UpdateRuntimeVar(ctx, projectID, "config", "a", aUpdate) }))
	bCreate := newRuntimeVar(projectID, "config", "b", projects.RuntimeVarTypeBoolean, "true", "")
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionCreate, "create: b", actor,
		func(ctx context.Context) error { return repo.CreateRuntimeVar(ctx, &bCreate) }))
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionDelete, "delete: a", actor,
		func(ctx context.Context) error { return repo.DeleteRuntimeVar(ctx, projectID, "config", "a") }))

	// 4 次写 = 4 版，revision 1..4 严格单调无空洞。
	versions, err := repo.ListVersions(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, versions, 4, "每次写恰好一版")
	revisions := make([]int64, 0, len(versions))
	for _, v := range versions {
		revisions = append(revisions, v.Revision)
	}
	require.Equal(t, []int64{4, 3, 2, 1}, revisions, "revision DESC 严格单调")

	// 版本元数据：action/summary/actor。
	require.Equal(t, projects.RuntimeVarActionDelete, versions[0].Action)
	require.Equal(t, "delete: a", versions[0].Summary)
	require.Equal(t, actor, versions[0].Actor)
	require.Equal(t, projects.RuntimeVarActionCreate, versions[3].Action)

	// 快照 = 写后 DB 状态（以 DB 为准，非内存态）：逐版校验。
	postCreateA, err := runtimeVarSnapshotJSON([]projects.RuntimeVar{aCreate})
	require.NoError(t, err)
	requireSnapshotEqual(t, versions[3].Vars, postCreateA)

	updatedA := aCreate
	updatedA.Value = "2"
	postUpdateA, err := runtimeVarSnapshotJSON([]projects.RuntimeVar{updatedA})
	require.NoError(t, err)
	requireSnapshotEqual(t, versions[2].Vars, postUpdateA)

	postCreateB := []projects.RuntimeVar{updatedA, bCreate}
	sort.Slice(postCreateB, func(i, j int) bool { return postCreateB[i].Key < postCreateB[j].Key })
	postCreateBSnap, err := runtimeVarSnapshotJSON(postCreateB)
	require.NoError(t, err)
	requireSnapshotEqual(t, versions[1].Vars, postCreateBSnap)

	postDeleteA := []projects.RuntimeVar{bCreate}
	postDeleteASnap, err := runtimeVarSnapshotJSON(postDeleteA)
	require.NoError(t, err)
	requireSnapshotEqual(t, versions[0].Vars, postDeleteASnap)

	// GetVersion 单查 + 不存在版本（含窗口外）(nil, nil)。
	ver2, err := repo.GetVersion(ctx, projectID, "config", 2)
	require.NoError(t, err)
	require.NotNil(t, ver2)
	require.EqualValues(t, 2, ver2.Revision)
	verGhost, err := repo.GetVersion(ctx, projectID, "config", 99)
	require.NoError(t, err)
	require.Nil(t, verGhost)

	// heads.revision 与版本链同步。
	headRev, found, err := repo.GetHead(ctx, projectID, "config")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 4, headRev)

	// 集合元数据变更：不 bump、不入版本链（D10）。
	require.NoError(t, repo.UpdateVarSet(ctx, projectID, "config", map[string]any{
		"visibility": projects.VarSetVisibilityPrivate,
		"updated_at": nowMicro(),
	}))
	headRev, found, err = repo.GetHead(ctx, projectID, "config")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 4, headRev, "元数据变更不 bump revision")
	versions, err = repo.ListVersions(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, versions, 4, "元数据变更不入版本链")
}

// --- 回滚 roundtrip（D11：全列快照，元数据不丢） ---

func TestRuntimeVarRepo_RollbackRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))

	// rev1: create a（description=d1）；rev2: update a（value=2, description=d2）；
	// rev3: create b。
	aCreate := newRuntimeVar(projectID, "config", "a", projects.RuntimeVarTypeInteger, "1", "d1")
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionCreate, "create: a", "admin:admin1",
		func(ctx context.Context) error { return repo.CreateRuntimeVar(ctx, &aCreate) }))
	origCreatedAt := aCreate.CreatedAt
	rollbackStartedAt := nowMicro()
	time.Sleep(time.Millisecond) // 保证回滚时刻 > 原 updated_at
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionUpdate, "update: a", "admin:admin1",
		func(ctx context.Context) error {
			return repo.UpdateRuntimeVar(ctx, projectID, "config", "a", map[string]any{
				"value":       "2",
				"description": "d2",
				"updated_at":  nowMicro(),
			})
		}))
	bCreate := newRuntimeVar(projectID, "config", "b", projects.RuntimeVarTypeBoolean, "true", "db")
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionCreate, "create: b", "admin:admin1",
		func(ctx context.Context) error { return repo.CreateRuntimeVar(ctx, &bCreate) }))

	// 回滚到 rev2（a=2，b 尚不存在）。
	require.NoError(t, runtimeVarRollbackFlow(ctx, db, repo, projectID, "config", 2, "admin:admin1"))

	// 值逐 key 等于目标快照；description/created_at 保留；updated_at=回滚时刻。
	vars, err := repo.ListRuntimeVars(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, vars, 1, "b 在回滚后消失")
	require.Equal(t, "a", vars[0].Key)
	require.Equal(t, "2", vars[0].Value)
	require.Equal(t, "d2", vars[0].Description, "description 从快照保留（D11 全列快照）")
	require.True(t, vars[0].CreatedAt.Equal(origCreatedAt), "created_at 从快照保留")
	require.True(t, vars[0].UpdatedAt.After(rollbackStartedAt), "updated_at = 回滚时刻")

	// 回滚产生新版本（rev4）：action=rollback，vars = 目标快照原样；可再回滚。
	versions, err := repo.ListVersions(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, versions, 4)
	require.Equal(t, projects.RuntimeVarActionRollback, versions[0].Action)
	require.Equal(t, "rollback to 2", versions[0].Summary)
	requireSnapshotEqual(t, versions[0].Vars, versions[2].Vars) // 回滚版本快照 = 目标快照原样
	headRev, found, err := repo.GetHead(ctx, projectID, "config")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 4, headRev)

	// 再回滚到 rev3（a=2 + b 存在）——回滚产生的版本自身可作回滚目标。
	require.NoError(t, runtimeVarRollbackFlow(ctx, db, repo, projectID, "config", 3, "admin:admin1"))
	vars, err = repo.ListRuntimeVars(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, vars, 2)
	require.Equal(t, "a", vars[0].Key)
	require.Equal(t, "b", vars[1].Key)

	// 回滚到已不存在的 revision：GetVersion (nil, nil)，flow 拒绝（app 层
	// 映射 NotFound "target revision pruned"）。
	err = runtimeVarRollbackFlow(ctx, db, repo, projectID, "config", 99, "admin:admin1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "pruned")
}

// --- 版本淘汰窗口（第 51 版写入时最老版被删，D11） ---

func TestRuntimeVarRepo_VersionPruning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))

	// 第一次 create + 50 次 update = 51 版。
	counter := newRuntimeVar(projectID, "config", "counter", projects.RuntimeVarTypeInteger, "0", "")
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionCreate, "create: counter", "apikey:key1",
		func(ctx context.Context) error { return repo.CreateRuntimeVar(ctx, &counter) }))
	for i := 1; i <= 50; i++ {
		n := i
		require.NoErrorf(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
			projects.RuntimeVarActionUpdate, fmt.Sprintf("update: counter=%d", n), "apikey:key1",
			func(ctx context.Context) error {
				return repo.UpdateRuntimeVar(ctx, projectID, "config", "counter", map[string]any{
					"value":      fmt.Sprintf("%d", n),
					"updated_at": nowMicro(),
				})
			}), "write #%d", n+1)
	}

	// 窗口边界：保留最近 50 版（revision 2..51），revision 1 被淘汰。
	versions, err := repo.ListVersions(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, versions, 50, "第 51 版写入后保留 50 版")
	require.EqualValues(t, 51, versions[0].Revision)
	require.EqualValues(t, 2, versions[len(versions)-1].Revision, "最老版（revision 1）被淘汰")
	pruned, err := repo.GetVersion(ctx, projectID, "config", 1)
	require.NoError(t, err)
	require.Nil(t, pruned, "回滚到已淘汰版本 found=false")
	surviving, err := repo.GetVersion(ctx, projectID, "config", 2)
	require.NoError(t, err)
	require.NotNil(t, surviving)

	// 回滚到窗口内最老版（rev2）成功，且产生 rev52 时 rev2 被淘汰。
	require.NoError(t, runtimeVarRollbackFlow(ctx, db, repo, projectID, "config", 2, "admin:admin1"))
	headRev, found, err := repo.GetHead(ctx, projectID, "config")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 52, headRev)
	pruned, err = repo.GetVersion(ctx, projectID, "config", 2)
	require.NoError(t, err)
	require.Nil(t, pruned, "回滚 bump 后 rev2 出窗被淘汰")
	versions, err = repo.ListVersions(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, versions, 50)
}

// --- 并发写（同集合串行化：版本链无空洞无重复） ---

func TestRuntimeVarRepo_ConcurrentWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))

	// 两个并发写事务（各自 LockHead → 写 → bump → 版本行）：FOR UPDATE 串行
	// 化，版本链 revision 1..2 无空洞无重复。
	const writers = 2
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key_%d", i)
			v := newRuntimeVar(projectID, "config", key, projects.RuntimeVarTypeString, fmt.Sprintf(`"v%d"`, i), "")
			<-start
			errs[i] = runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
				projects.RuntimeVarActionCreate, "create: "+key, "admin:admin1",
				func(ctx context.Context) error { return repo.CreateRuntimeVar(ctx, &v) })
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoErrorf(t, err, "writer %d", i)
	}

	headRev, found, err := repo.GetHead(ctx, projectID, "config")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, writers, headRev)

	versions, err := repo.ListVersions(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, versions, writers, "并发写无丢版")
	gotRevisions := map[int64]bool{}
	for _, v := range versions {
		require.False(t, gotRevisions[v.Revision], "revision 无重复: %d", v.Revision)
		gotRevisions[v.Revision] = true
	}
	for r := int64(1); r <= writers; r++ {
		require.True(t, gotRevisions[r], "revision 无空洞: %d 缺失", r)
	}

	vars, err := repo.ListRuntimeVars(ctx, projectID, "config")
	require.NoError(t, err)
	require.Len(t, vars, writers)
}

// --- footprint（D13 限额度量） ---

func TestRuntimeVarRepo_Footprint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))

	// 空集：(0, 0)。
	count, bytes, err := repo.Footprint(ctx, projectID, "config")
	require.NoError(t, err)
	require.Zero(t, count)
	require.Zero(t, bytes)

	// `"hello"`=7、`42`=2、`true`=4（JSONB 归一化文本字节数）。
	for _, spec := range []struct {
		key, value string
	}{
		{"s", `"hello"`},
		{"i", `42`},
		{"b", `true`},
	} {
		v := newRuntimeVar(projectID, "config", spec.key, projects.RuntimeVarTypeString, spec.value, "")
		require.NoError(t, repo.CreateRuntimeVar(ctx, &v))
	}
	count, bytes, err = repo.Footprint(ctx, projectID, "config")
	require.NoError(t, err)
	require.Equal(t, 3, count)
	require.EqualValues(t, 7+2+4, bytes, "total_bytes = octet_length(value::text) 总和")

	require.NoError(t, repo.DeleteRuntimeVar(ctx, projectID, "config", "s"))
	count, bytes, err = repo.Footprint(ctx, projectID, "config")
	require.NoError(t, err)
	require.Equal(t, 2, count)
	require.EqualValues(t, 2+4, bytes)
}

// --- 项目删除 CASCADE 四表（D1：FK 链两级传导） ---

func TestRuntimeVarRepo_ProjectDeleteCascade(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewRuntimeVarRepository(db)
	vs := newVarSet(t, projectID, "config", projects.VarSetVisibilityPublic)
	require.NoError(t, repo.CreateVarSet(ctx, &vs))
	a := newRuntimeVar(projectID, "config", "a", projects.RuntimeVarTypeString, `"va"`, "")
	require.NoError(t, runtimeVarWriteFlow(ctx, db, repo, projectID, "config",
		projects.RuntimeVarActionCreate, "create: a", "admin:admin1",
		func(ctx context.Context) error { return repo.CreateRuntimeVar(ctx, &a) }))

	require.Equal(t, 1, runtimeVarTableCount(ctx, t, db, "runtime_var_sets", projectID))
	require.Equal(t, 1, runtimeVarTableCount(ctx, t, db, "runtime_vars", projectID))
	require.Equal(t, 1, runtimeVarTableCount(ctx, t, db, "runtime_var_heads", projectID))
	require.Equal(t, 1, runtimeVarTableCount(ctx, t, db, "runtime_var_versions", projectID))

	// 删除项目行 → 四表经 FK CASCADE 清零（不进 DeleteProjectControlPlaneRows）。
	_, err := db.ExecContext(ctx, "DELETE FROM projects WHERE id = ?", projectID)
	require.NoError(t, err)

	require.Zero(t, runtimeVarTableCount(ctx, t, db, "runtime_var_sets", projectID))
	require.Zero(t, runtimeVarTableCount(ctx, t, db, "runtime_vars", projectID))
	require.Zero(t, runtimeVarTableCount(ctx, t, db, "runtime_var_heads", projectID))
	require.Zero(t, runtimeVarTableCount(ctx, t, db, "runtime_var_versions", projectID))
}
