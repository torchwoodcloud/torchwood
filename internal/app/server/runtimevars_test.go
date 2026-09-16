package server

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖 docs/design/runtime-vars.md §6「app/server 单测」段：格式拒、
// 值构造（JSON 合法性/深度/尺寸）、限额全项、409/404、回滚语义、可选字段
// 不改语义，以及写路径规范调用序（fake 记录调用序断言）。

// fakeRuntimeVarRepo 是 RuntimeVarRepository 的内存实现：模拟 PG 语义
// （主键冲突 = SQLSTATE 23505 错误文本、Update 列白名单、版本淘汰窗口 50）
// 并记录方法调用序（规范调用序断言用）。
type fakeRuntimeVarRepo struct {
	mu       sync.Mutex
	calls    []string
	sets     map[string]*projects.VarSet                // key: project/varSet
	heads    map[string]int64                           // key: project/varSet
	vars     map[string]map[string]*projects.RuntimeVar // key: project/varSet -> key -> var
	versions map[string][]*projects.RuntimeVarVersion   // key: project/varSet（append 序 = revision 序）
}

func newFakeRuntimeVarRepo() *fakeRuntimeVarRepo {
	return &fakeRuntimeVarRepo{
		sets:     map[string]*projects.VarSet{},
		heads:    map[string]int64{},
		vars:     map[string]map[string]*projects.RuntimeVar{},
		versions: map[string][]*projects.RuntimeVarVersion{},
	}
}

func fakeRuntimeVarKey(projectID, varSetID string) string { return projectID + "/" + varSetID }

func (f *fakeRuntimeVarRepo) record(method string) { f.calls = append(f.calls, method) }

// callOrder 返回 method 在调用序中的位置（未调用 = -1），供相对序断言。
func (f *fakeRuntimeVarRepo) callOrder(method string) int {
	for i, c := range f.calls {
		if c == method {
			return i
		}
	}
	return -1
}

func (f *fakeRuntimeVarRepo) lastVersion(projectID, varSetID string) *projects.RuntimeVarVersion {
	vs := f.versions[fakeRuntimeVarKey(projectID, varSetID)]
	if len(vs) == 0 {
		return nil
	}
	return vs[len(vs)-1]
}

func (f *fakeRuntimeVarRepo) CreateVarSet(ctx context.Context, vs *projects.VarSet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateVarSet")
	k := fakeRuntimeVarKey(vs.ProjectID, vs.VarSetID)
	if _, ok := f.sets[k]; ok {
		return fmt.Errorf(`SQLSTATE 23505: duplicate key value violates unique constraint "runtime_var_sets_pkey"`)
	}
	cp := *vs
	f.sets[k] = &cp
	f.heads[k] = 0
	return nil
}

func (f *fakeRuntimeVarRepo) GetVarSet(ctx context.Context, projectID, varSetID string) (*projects.VarSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetVarSet")
	vs, ok := f.sets[fakeRuntimeVarKey(projectID, varSetID)]
	if !ok {
		return nil, nil
	}
	cp := *vs
	return &cp, nil
}

func (f *fakeRuntimeVarRepo) ListVarSets(ctx context.Context, projectID string) ([]projects.VarSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListVarSets")
	var out []projects.VarSet
	for k, vs := range f.sets {
		if strings.HasPrefix(k, projectID+"/") {
			out = append(out, *vs)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].VarSetID < out[j].VarSetID
	})
	return out, nil
}

func (f *fakeRuntimeVarRepo) UpdateVarSet(ctx context.Context, projectID, varSetID string, cols map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpdateVarSet")
	vs, ok := f.sets[fakeRuntimeVarKey(projectID, varSetID)]
	if !ok {
		return fmt.Errorf("var set not found")
	}
	allowed := map[string]struct{}{"visibility": {}, "description": {}, "updated_at": {}}
	for col := range cols {
		if _, ok := allowed[col]; !ok {
			return fmt.Errorf("column %q not updatable", col)
		}
	}
	if v, ok := cols["visibility"]; ok {
		vs.Visibility = v.(string)
	}
	if v, ok := cols["description"]; ok {
		vs.Description = v.(string)
	}
	if v, ok := cols["updated_at"]; ok {
		vs.UpdatedAt = v.(time.Time)
	}
	return nil
}

func (f *fakeRuntimeVarRepo) DeleteVarSet(ctx context.Context, projectID, varSetID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteVarSet")
	k := fakeRuntimeVarKey(projectID, varSetID)
	delete(f.sets, k)
	delete(f.heads, k)
	delete(f.vars, k)
	delete(f.versions, k)
	return nil
}

func (f *fakeRuntimeVarRepo) GetHead(ctx context.Context, projectID, varSetID string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetHead")
	rev, ok := f.heads[fakeRuntimeVarKey(projectID, varSetID)]
	return rev, ok, nil
}

func (f *fakeRuntimeVarRepo) LockHead(ctx context.Context, projectID, varSetID string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("LockHead")
	rev, ok := f.heads[fakeRuntimeVarKey(projectID, varSetID)]
	return rev, ok, nil
}

func (f *fakeRuntimeVarRepo) BumpRevisionAndPrune(ctx context.Context, projectID, varSetID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("BumpRevisionAndPrune")
	k := fakeRuntimeVarKey(projectID, varSetID)
	rev, ok := f.heads[k]
	if !ok {
		return 0, fmt.Errorf("head not found: bump requires LockHead in the same transaction")
	}
	rev++
	f.heads[k] = rev
	// 淘汰窗口：保留最近 50 版。
	if vs := f.versions[k]; len(vs) > 50 {
		f.versions[k] = vs[len(vs)-50:]
	}
	return rev, nil
}

func (f *fakeRuntimeVarRepo) CreateRuntimeVar(ctx context.Context, v *projects.RuntimeVar) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateRuntimeVar")
	k := fakeRuntimeVarKey(v.ProjectID, v.VarSetID)
	if f.vars[k] == nil {
		f.vars[k] = map[string]*projects.RuntimeVar{}
	}
	if _, ok := f.vars[k][v.Key]; ok {
		return fmt.Errorf(`SQLSTATE 23505: duplicate key value violates unique constraint "runtime_vars_pkey"`)
	}
	cp := *v
	f.vars[k][v.Key] = &cp
	return nil
}

func (f *fakeRuntimeVarRepo) GetRuntimeVar(ctx context.Context, projectID, varSetID, key string) (*projects.RuntimeVar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetRuntimeVar")
	m := f.vars[fakeRuntimeVarKey(projectID, varSetID)]
	if m == nil {
		return nil, nil
	}
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	cp := *v
	return &cp, nil
}

func (f *fakeRuntimeVarRepo) ListRuntimeVars(ctx context.Context, projectID, varSetID string) ([]projects.RuntimeVar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListRuntimeVars")
	m := f.vars[fakeRuntimeVarKey(projectID, varSetID)]
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]projects.RuntimeVar, 0, len(keys))
	for _, key := range keys {
		out = append(out, *m[key])
	}
	return out, nil
}

func (f *fakeRuntimeVarRepo) UpdateRuntimeVar(ctx context.Context, projectID, varSetID, key string, cols map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpdateRuntimeVar")
	m := f.vars[fakeRuntimeVarKey(projectID, varSetID)]
	if m == nil {
		return fmt.Errorf("var set not found")
	}
	v, ok := m[key]
	if !ok {
		return fmt.Errorf("var not found")
	}
	allowed := map[string]struct{}{"value": {}, "value_type": {}, "description": {}, "updated_at": {}}
	for col := range cols {
		if _, ok := allowed[col]; !ok {
			return fmt.Errorf("column %q not updatable", col)
		}
	}
	if val, ok := cols["value"]; ok {
		v.Value = val.(string)
	}
	if val, ok := cols["value_type"]; ok {
		v.ValueType = val.(string)
	}
	if val, ok := cols["description"]; ok {
		v.Description = val.(string)
	}
	return nil
}

func (f *fakeRuntimeVarRepo) DeleteRuntimeVar(ctx context.Context, projectID, varSetID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteRuntimeVar")
	delete(f.vars[fakeRuntimeVarKey(projectID, varSetID)], key)
	return nil
}

func (f *fakeRuntimeVarRepo) Footprint(ctx context.Context, projectID, varSetID string) (int, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Footprint")
	m := f.vars[fakeRuntimeVarKey(projectID, varSetID)]
	var total int64
	for _, v := range m {
		total += int64(len(v.Value))
	}
	return len(m), total, nil
}

func (f *fakeRuntimeVarRepo) ReplaceVars(ctx context.Context, projectID, varSetID string, vars []projects.RuntimeVar) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ReplaceVars")
	k := fakeRuntimeVarKey(projectID, varSetID)
	f.vars[k] = map[string]*projects.RuntimeVar{}
	for i := range vars {
		cp := vars[i]
		f.vars[k][vars[i].Key] = &cp
	}
	return nil
}

func (f *fakeRuntimeVarRepo) InsertVersion(ctx context.Context, v *projects.RuntimeVarVersion) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("InsertVersion")
	k := fakeRuntimeVarKey(v.ProjectID, v.VarSetID)
	for _, existing := range f.versions[k] {
		if existing.Revision == v.Revision {
			return fmt.Errorf(`SQLSTATE 23505: duplicate key value violates unique constraint "runtime_var_versions_pkey"`)
		}
	}
	cp := *v
	f.versions[k] = append(f.versions[k], &cp)
	return nil
}

func (f *fakeRuntimeVarRepo) GetVersion(ctx context.Context, projectID, varSetID string, revision int64) (*projects.RuntimeVarVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetVersion")
	for _, v := range f.versions[fakeRuntimeVarKey(projectID, varSetID)] {
		if v.Revision == revision {
			cp := *v
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeRuntimeVarRepo) ListVersions(ctx context.Context, projectID, varSetID string) ([]projects.RuntimeVarVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListVersions")
	vs := f.versions[fakeRuntimeVarKey(projectID, varSetID)]
	out := make([]projects.RuntimeVarVersion, len(vs))
	for i := range vs {
		out[i] = *vs[i]
	}
	// revision DESC。
	sort.Slice(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	return out, nil
}

// stubRunner 是 uow.Runner 的直通实现（单测无真事务；repo 的 fake 自带
// 一致性，规范调用序由 calls 记录承担）。
type stubRunner struct{}

func (stubRunner) Run(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

func newRuntimeVarsForTest() (*RuntimeVars, *fakeRuntimeVarRepo) {
	repo := newFakeRuntimeVarRepo()
	return NewRuntimeVars(repo, stubRunner{}), repo
}

func mustCreateVarSetForTest(t *testing.T, r *RuntimeVars, projectID, varSetID string) *VarSetView {
	t.Helper()
	view, err := r.CreateVarSet(context.Background(), CreateVarSetCommand{
		ProjectID: projectID, VarSetID: varSetID,
	})
	require.NoError(t, err)
	return view
}

// --- 值构造器 ---

func TestRuntimeVarsValueConstructors(t *testing.T) {
	t.Parallel()
	s := StringVarValue("he\"llo")
	require.Equal(t, projects.RuntimeVarTypeString, s.ValueType)
	require.Equal(t, `"he\"llo"`, s.Value)

	i := IntegerVarValue(-42)
	require.Equal(t, "-42", i.Value)

	f, err := FloatVarValue(3.14)
	require.NoError(t, err)
	require.Equal(t, projects.RuntimeVarTypeFloat, f.ValueType)
	require.JSONEq(t, "3.14", f.Value)

	b := BoolVarValue(true)
	require.Equal(t, "true", b.Value)

	j, err := JSONVarValue(`{"a": 1, "b": [true, null]}`)
	require.NoError(t, err)
	require.Equal(t, projects.RuntimeVarTypeJSON, j.ValueType)
	require.Equal(t, `{"a": 1, "b": [true, null]}`, j.Value)
}

func TestRuntimeVarsJSONVarValueRejects(t *testing.T) {
	t.Parallel()
	// 非法 JSON。
	_, err := JSONVarValue(`{"a": `)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	// 空串。
	_, err = JSONVarValue(``)
	require.Error(t, err)
	// 尾随垃圾。
	_, err = JSONVarValue(`{"a":1} x`)
	require.Error(t, err)
	// 深度 101 拒（D3）。
	deep := strings.Repeat("[", 101) + strings.Repeat("]", 101)
	_, err = JSONVarValue(deep)
	require.Error(t, err)
	require.Contains(t, err.Error(), "depth")
	// 深度恰 100 过。
	okDeep := strings.Repeat("[", 100) + strings.Repeat("]", 100)
	_, err = JSONVarValue(okDeep)
	require.NoError(t, err)
}

// --- 格式拒 ---

func TestRuntimeVarsFormatRejects(t *testing.T) {
	t.Parallel()
	r, _ := newRuntimeVarsForTest()
	ctx := context.Background()

	for _, bad := range []string{"", "Upper", "1abc", "has-dash", "has.dot", strings.Repeat("a", 41)} {
		_, err := r.CreateVarSet(ctx, CreateVarSetCommand{ProjectID: "p1", VarSetID: bad})
		require.Error(t, err, "var_set_id %q 应被拒", bad)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}

	mustCreateVarSetForTest(t, r, "p1", "set1")
	for _, bad := range []string{"", "Key", "1abc", "has-dash", strings.Repeat("a", 65)} {
		_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
			ProjectID: "p1", VarSetID: "set1", Key: bad, Value: StringVarValue("v"),
		})
		require.Error(t, err, "key %q 应被拒", bad)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	// 合法边界：恰 64 字符 key、恰 40 字符 var_set_id。
	_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: strings.Repeat("a", 64), Value: StringVarValue("v"),
	})
	require.NoError(t, err)
	_, err = r.CreateVarSet(ctx, CreateVarSetCommand{ProjectID: "p1", VarSetID: strings.Repeat("b", 40)})
	require.NoError(t, err)
}

// --- 集合限额（20/项目）与冲突 409 ---

func TestRuntimeVarsSetLimitAndConflict(t *testing.T) {
	t.Parallel()
	r, _ := newRuntimeVarsForTest()
	ctx := context.Background()

	for i := 0; i < RuntimeVarsMaxSetsPerProject; i++ {
		mustCreateVarSetForTest(t, r, "p1", "set"+strconv.Itoa(i))
	}
	_, err := r.CreateVarSet(ctx, CreateVarSetCommand{ProjectID: "p1", VarSetID: "overflow"})
	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, err.Error(), "20")

	// 冲突 409。
	_, err = r.CreateVarSet(ctx, CreateVarSetCommand{ProjectID: "p1", VarSetID: "set0"})
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	// 其他项目不受影响。
	_, err = r.CreateVarSet(ctx, CreateVarSetCommand{ProjectID: "p2", VarSetID: "set0"})
	require.NoError(t, err)
}

// --- var 限额（500 keys/集、单值 64KB、总值 1MB）与 409/404 ---

func TestRuntimeVarsKeyCountLimit(t *testing.T) {
	t.Parallel()
	r, _ := newRuntimeVarsForTest()
	ctx := context.Background()
	mustCreateVarSetForTest(t, r, "p1", "set1")

	for i := 0; i < RuntimeVarsMaxKeysPerSet; i++ {
		_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
			ProjectID: "p1", VarSetID: "set1",
			Key: "k" + strconv.Itoa(i), Value: IntegerVarValue(int64(i)),
		})
		require.NoError(t, err)
	}
	_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "overflow", Value: IntegerVarValue(0),
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, err.Error(), strconv.Itoa(RuntimeVarsMaxKeysPerSet))
}

func TestRuntimeVarsValueSizeLimit(t *testing.T) {
	t.Parallel()
	r, _ := newRuntimeVarsForTest()
	ctx := context.Background()
	mustCreateVarSetForTest(t, r, "p1", "set1")

	// 单值 64KB：JSON 文本 65537 字节（65535 字符 + 2 引号）拒。
	_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "big", Value: StringVarValue(strings.Repeat("a", RuntimeVarsMaxValueBytes-1)),
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	// 恰 65534 字符 + 2 引号 = 65536 过。
	_, err = r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "fit", Value: StringVarValue(strings.Repeat("b", RuntimeVarsMaxValueBytes-2)),
	})
	require.NoError(t, err)
}

func TestRuntimeVarsTotalSizeLimit(t *testing.T) {
	t.Parallel()
	r, _ := newRuntimeVarsForTest()
	ctx := context.Background()
	mustCreateVarSetForTest(t, r, "p1", "set1")

	// 16 × 62002 字节 = 992032 ≤ 1MB；第 17 个（62002 字节）投影 1054034
	// 超限拒。
	chunk := StringVarValue(strings.Repeat("c", 62000))
	for i := 0; i < 16; i++ {
		_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
			ProjectID: "p1", VarSetID: "set1", Key: "k" + strconv.Itoa(i), Value: chunk,
		})
		require.NoError(t, err)
	}
	_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "overflow", Value: chunk,
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, err.Error(), "total size")

	// 删一个大值后小值可写（Footprint 实时度量）。
	require.NoError(t, r.DeleteRuntimeVar(ctx, "p1", "set1", "k0", "admin:admin1"))
	_, err = r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "tiny", Value: IntegerVarValue(1),
	})
	require.NoError(t, err)
}

func TestRuntimeVarsVarConflictAndMissing(t *testing.T) {
	t.Parallel()
	r, _ := newRuntimeVarsForTest()
	ctx := context.Background()
	mustCreateVarSetForTest(t, r, "p1", "set1")

	// 重复 key → 409。
	_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "dup", Value: StringVarValue("v"),
	})
	require.NoError(t, err)
	_, err = r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "dup", Value: StringVarValue("v2"),
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	// 集合不存在 → 404（写路径经 LockHead found=false）。
	_, err = r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "ghost", Key: "k", Value: StringVarValue("v"),
	})
	require.Equal(t, codes.NotFound, status.Code(err))

	// var 不存在 → 404。
	_, err = r.UpdateRuntimeVar(ctx, UpdateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "ghost", Value: StringVarValue("v"),
	})
	require.Equal(t, codes.NotFound, status.Code(err))
	err = r.DeleteRuntimeVar(ctx, "p1", "set1", "ghost", "admin:admin1")
	require.Equal(t, codes.NotFound, status.Code(err))

	// 集合缺失的读 → 404。
	_, err = r.GetRuntimeVar(ctx, "p1", "ghost", "k")
	require.Equal(t, codes.NotFound, status.Code(err))
}

// --- 集合元数据：optional 不改语义 + 不入版本链（D10） ---

func TestRuntimeVarsUpdateVarSetOptionalSemantics(t *testing.T) {
	t.Parallel()
	r, repo := newRuntimeVarsForTest()
	ctx := context.Background()
	view := mustCreateVarSetForTest(t, r, "p1", "set1")
	require.Equal(t, projects.VarSetVisibilityPublic, view.VarSet.Visibility)

	created, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: IntegerVarValue(1), Description: "d1",
	})
	require.NoError(t, err)
	require.Len(t, repo.versions["p1/set1"], 1)

	// 只改 visibility：description 不动。
	priv := projects.VarSetVisibilityPrivate
	updated, err := r.UpdateVarSet(ctx, UpdateVarSetCommand{ProjectID: "p1", VarSetID: "set1", Visibility: &priv})
	require.NoError(t, err)
	require.Equal(t, priv, updated.VarSet.Visibility)
	require.Empty(t, updated.VarSet.Description)

	// 元数据变更不 bump revision、不入版本链（D10）：var 写已 bump 到 1，
	// 元数据更新后保持 1。
	require.Len(t, repo.versions["p1/set1"], 1)
	require.EqualValues(t, 1, updated.Revision)

	// 集合缺失 → 404。
	_, err = r.UpdateVarSet(ctx, UpdateVarSetCommand{ProjectID: "p1", VarSetID: "ghost"})
	require.Equal(t, codes.NotFound, status.Code(err))

	// var 元数据 optional 不改语义：Update 不带 description 时保留。
	_, err = r.UpdateRuntimeVar(ctx, UpdateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: IntegerVarValue(2),
	})
	require.NoError(t, err)
	got, err := r.GetRuntimeVar(ctx, "p1", "set1", "a")
	require.NoError(t, err)
	require.Equal(t, "d1", got.Description, "未设置 description = 不修改")
	require.Equal(t, "2", got.Value)
	require.Equal(t, created.CreatedAt, got.CreatedAt, "created_at 不随 Update 变")

	// 带 description 时替换。
	newDesc := "changed"
	_, err = r.UpdateRuntimeVar(ctx, UpdateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: StringVarValue("s"), Description: &newDesc,
	})
	require.NoError(t, err)
	got, err = r.GetRuntimeVar(ctx, "p1", "set1", "a")
	require.NoError(t, err)
	require.Equal(t, "changed", got.Description)
	require.Equal(t, projects.RuntimeVarTypeString, got.ValueType, "类型可随 Update 显式变更")
}

// --- 写路径规范调用序（§2.5：LockHead → 变更 → 快照回读 → InsertVersion → Bump） ---

func TestRuntimeVarsWriteFlowCanonicalOrder(t *testing.T) {
	t.Parallel()
	r, repo := newRuntimeVarsForTest()
	ctx := context.Background()
	mustCreateVarSetForTest(t, r, "p1", "set1")

	_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: IntegerVarValue(1), Actor: "admin:admin1",
	})
	require.NoError(t, err)

	// 规范调用序：LockHead 先于变更，InsertVersion(rev=locked+1) 先于 Bump。
	require.Less(t, repo.callOrder("LockHead"), repo.callOrder("CreateRuntimeVar"))
	require.Less(t, repo.callOrder("CreateRuntimeVar"), repo.callOrder("ListRuntimeVars"), "快照从写后回读")
	require.Less(t, repo.callOrder("ListRuntimeVars"), repo.callOrder("InsertVersion"))
	require.Less(t, repo.callOrder("InsertVersion"), repo.callOrder("BumpRevisionAndPrune"), "淘汰窗口计入本次新版本行")

	// 版本行：revision = locked(0)+1，action/summary/actor。
	ver := repo.lastVersion("p1", "set1")
	require.EqualValues(t, 1, ver.Revision)
	require.Equal(t, projects.RuntimeVarActionCreate, ver.Action)
	require.Equal(t, "create: a", ver.Summary)
	require.Equal(t, "admin:admin1", ver.Actor)

	// Update/Delete 同序。
	repo.calls = nil
	_, err = r.UpdateRuntimeVar(ctx, UpdateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: IntegerVarValue(2), Actor: "apikey:k1",
	})
	require.NoError(t, err)
	require.Less(t, repo.callOrder("LockHead"), repo.callOrder("UpdateRuntimeVar"))
	require.Less(t, repo.callOrder("UpdateRuntimeVar"), repo.callOrder("ListRuntimeVars"))
	require.Less(t, repo.callOrder("InsertVersion"), repo.callOrder("BumpRevisionAndPrune"))
	ver = repo.lastVersion("p1", "set1")
	require.EqualValues(t, 2, ver.Revision)
	require.Equal(t, "update: a", ver.Summary)
	require.Equal(t, "apikey:k1", ver.Actor)

	repo.calls = nil
	require.NoError(t, r.DeleteRuntimeVar(ctx, "p1", "set1", "a", "apikey:k1"))
	require.Less(t, repo.callOrder("LockHead"), repo.callOrder("DeleteRuntimeVar"))
	require.Less(t, repo.callOrder("InsertVersion"), repo.callOrder("BumpRevisionAndPrune"))
	ver = repo.lastVersion("p1", "set1")
	require.Equal(t, projects.RuntimeVarActionDelete, ver.Action)
	require.Equal(t, "delete: a", ver.Summary)

	// 快照内容 = 写后状态（delete 后为空集 {}）。
	require.JSONEq(t, "{}", ver.Vars)
}

// --- 版本链读取 ---

func TestRuntimeVarsVersionReads(t *testing.T) {
	t.Parallel()
	r, _ := newRuntimeVarsForTest()
	ctx := context.Background()
	mustCreateVarSetForTest(t, r, "p1", "set1")

	_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: IntegerVarValue(1), Description: "d1",
	})
	require.NoError(t, err)

	versions, err := r.ListRuntimeVarVersions(ctx, "p1", "set1")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.EqualValues(t, 1, versions[0].Revision)

	detail, err := r.GetRuntimeVarVersion(ctx, "p1", "set1", 1)
	require.NoError(t, err)
	require.Len(t, detail.Vars, 1)
	require.Equal(t, "a", detail.Vars[0].Key)
	require.Equal(t, "d1", detail.Vars[0].Description)
	require.Equal(t, "1", detail.Vars[0].Value)

	// 不存在版本 / 集合 → 404。
	_, err = r.GetRuntimeVarVersion(ctx, "p1", "set1", 99)
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = r.ListRuntimeVarVersions(ctx, "p1", "ghost")
	require.Equal(t, codes.NotFound, status.Code(err))
}

// --- 回滚（§2.5：目标读取 → 整替 → rollback 版本（快照原样）→ bump） ---

func TestRuntimeVarsRollback(t *testing.T) {
	t.Parallel()
	r, repo := newRuntimeVarsForTest()
	ctx := context.Background()
	mustCreateVarSetForTest(t, r, "p1", "set1")

	created, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: IntegerVarValue(1), Description: "d1",
	})
	require.NoError(t, err)
	_, err = r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "b", Value: BoolVarValue(true),
	})
	require.NoError(t, err)
	_, err = r.UpdateRuntimeVar(ctx, UpdateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: IntegerVarValue(2), Actor: "admin:admin1",
	})
	require.NoError(t, err)
	// 当前 revision 3。

	// 回滚到当前 → InvalidArgument（D12）。
	_, err = r.Rollback(ctx, "p1", "set1", 3, "admin:admin1")
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 回滚到不存在/淘汰版本 → NotFound "pruned"。
	_, err = r.Rollback(ctx, "p1", "set1", 99, "admin:admin1")
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Contains(t, err.Error(), "pruned")

	// 回滚到 rev1（仅 a=1）：整替 + 新版本 rev4（快照 = rev1 原样）。
	rolled, err := r.Rollback(ctx, "p1", "set1", 1, "admin:admin1")
	require.NoError(t, err)
	require.EqualValues(t, 4, rolled.Revision)
	require.Equal(t, projects.RuntimeVarActionRollback, rolled.Action)
	require.Equal(t, "rollback to 1", rolled.Summary)
	require.Equal(t, "admin:admin1", rolled.Actor)

	// 值逐 key 等于目标快照；b 消失；description/created_at 保留（D11）。
	vars, err := r.ListRuntimeVars(ctx, "p1", "set1")
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, "a", vars[0].Key)
	require.Equal(t, "1", vars[0].Value)
	require.Equal(t, "d1", vars[0].Description)
	require.True(t, vars[0].CreatedAt.Equal(created.CreatedAt), "created_at 从快照保留")

	// 回滚版本快照 = 目标快照原样。
	rev1 := repo.versions["p1/set1"][0]
	require.Equal(t, rev1.Vars, rolled.Vars)

	// 集合缺失 → 404。
	_, err = r.Rollback(ctx, "p1", "ghost", 1, "admin:admin1")
	require.Equal(t, codes.NotFound, status.Code(err))
}

// --- 视图（etag/revision/footprint 投影，D2） ---

func TestRuntimeVarsViewETag(t *testing.T) {
	t.Parallel()
	r, _ := newRuntimeVarsForTest()
	ctx := context.Background()

	view := mustCreateVarSetForTest(t, r, "p1", "set1")
	require.Equal(t, view.VarSet.Epoch+":0", view.ETag)
	require.Zero(t, view.Revision)
	require.Zero(t, view.VarCount)

	_, err := r.CreateRuntimeVar(ctx, CreateRuntimeVarCommand{
		ProjectID: "p1", VarSetID: "set1", Key: "a", Value: StringVarValue("hello"),
	})
	require.NoError(t, err)

	got, err := r.GetVarSet(ctx, "p1", "set1")
	require.NoError(t, err)
	require.Equal(t, view.VarSet.Epoch+":1", got.ETag)
	require.EqualValues(t, 1, got.Revision)
	require.Equal(t, 1, got.VarCount)
	require.EqualValues(t, 7, got.TotalBytes) // `"hello"` = 7 字节

	// 缺失 → 404。
	_, err = r.GetVarSet(ctx, "p1", "ghost")
	require.Equal(t, codes.NotFound, status.Code(err))
	err = r.DeleteVarSet(ctx, "p1", "ghost")
	require.Equal(t, codes.NotFound, status.Code(err))
}
