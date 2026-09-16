package servergrpc

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件是 RuntimeVarsService handler 的单元测试（docs/design/runtime-vars.md
// §6「handler 单测」段）：proto ↔ domain 映射、错误透传、audit resource 标注、
// actor 前缀派生。app 用例注入内存 fake repo（无需 DB）。

// fakeRuntimeVarHandlerRepo 只实现 handler 单测触达的 repo 方法（其余经
// 嵌入的 nil interface panic——被触达即测试失败，防静默偏离预期路径）。
type fakeRuntimeVarHandlerRepo struct {
	projects.RuntimeVarRepository

	sets        map[string]*projects.VarSet
	heads       map[string]int64
	vars        map[string]map[string]*projects.RuntimeVar
	lastVersion *projects.RuntimeVarVersion // 最近一次 InsertVersion（actor 断言用）
}

func newFakeRuntimeVarHandlerRepo() *fakeRuntimeVarHandlerRepo {
	return &fakeRuntimeVarHandlerRepo{
		sets:  map[string]*projects.VarSet{},
		heads: map[string]int64{},
		vars:  map[string]map[string]*projects.RuntimeVar{},
	}
}

// errRuntimeVarFakeMissing 是 fake 的缺行哨兵（缺行不应发生——app 层先做
// 存在性检查）。
var errRuntimeVarFakeMissing = status.Error(codes.Internal, "fake: row missing")

func (f *fakeRuntimeVarHandlerRepo) key(projectID, varSetID string) string {
	return projectID + "/" + varSetID
}

func (f *fakeRuntimeVarHandlerRepo) CreateVarSet(ctx context.Context, vs *projects.VarSet) error {
	cp := *vs
	f.sets[f.key(vs.ProjectID, vs.VarSetID)] = &cp
	f.heads[f.key(vs.ProjectID, vs.VarSetID)] = 0
	return nil
}

func (f *fakeRuntimeVarHandlerRepo) GetVarSet(ctx context.Context, projectID, varSetID string) (*projects.VarSet, error) {
	vs, ok := f.sets[f.key(projectID, varSetID)]
	if !ok {
		return nil, nil
	}
	cp := *vs
	return &cp, nil
}

func (f *fakeRuntimeVarHandlerRepo) ListVarSets(ctx context.Context, projectID string) ([]projects.VarSet, error) {
	var out []projects.VarSet
	for k, vs := range f.sets {
		if strings.HasPrefix(k, projectID+"/") {
			out = append(out, *vs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VarSetID < out[j].VarSetID })
	return out, nil
}

func (f *fakeRuntimeVarHandlerRepo) UpdateVarSet(ctx context.Context, projectID, varSetID string, cols map[string]any) error {
	vs, ok := f.sets[f.key(projectID, varSetID)]
	if !ok {
		return errRuntimeVarFakeMissing
	}
	if v, ok := cols["visibility"]; ok {
		vs.Visibility = v.(string)
	}
	if v, ok := cols["description"]; ok {
		vs.Description = v.(string)
	}
	return nil
}

func (f *fakeRuntimeVarHandlerRepo) GetHead(ctx context.Context, projectID, varSetID string) (int64, bool, error) {
	rev, ok := f.heads[f.key(projectID, varSetID)]
	return rev, ok, nil
}

func (f *fakeRuntimeVarHandlerRepo) LockHead(ctx context.Context, projectID, varSetID string) (int64, bool, error) {
	rev, ok := f.heads[f.key(projectID, varSetID)]
	return rev, ok, nil
}

func (f *fakeRuntimeVarHandlerRepo) Footprint(ctx context.Context, projectID, varSetID string) (int, int64, error) {
	m := f.vars[f.key(projectID, varSetID)]
	var total int64
	for _, v := range m {
		total += int64(len(v.Value))
	}
	return len(m), total, nil
}

func (f *fakeRuntimeVarHandlerRepo) CreateRuntimeVar(ctx context.Context, v *projects.RuntimeVar) error {
	k := f.key(v.ProjectID, v.VarSetID)
	if f.vars[k] == nil {
		f.vars[k] = map[string]*projects.RuntimeVar{}
	}
	cp := *v
	f.vars[k][v.Key] = &cp
	return nil
}

func (f *fakeRuntimeVarHandlerRepo) GetRuntimeVar(ctx context.Context, projectID, varSetID, key string) (*projects.RuntimeVar, error) {
	m := f.vars[f.key(projectID, varSetID)]
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

func (f *fakeRuntimeVarHandlerRepo) ListRuntimeVars(ctx context.Context, projectID, varSetID string) ([]projects.RuntimeVar, error) {
	m := f.vars[f.key(projectID, varSetID)]
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

func (f *fakeRuntimeVarHandlerRepo) UpdateRuntimeVar(ctx context.Context, projectID, varSetID, key string, cols map[string]any) error {
	m := f.vars[f.key(projectID, varSetID)]
	if m == nil || m[key] == nil {
		return errRuntimeVarFakeMissing
	}
	if v, ok := cols["value"]; ok {
		m[key].Value = v.(string)
	}
	if v, ok := cols["value_type"]; ok {
		m[key].ValueType = v.(string)
	}
	if v, ok := cols["description"]; ok {
		m[key].Description = v.(string)
	}
	return nil
}

func (f *fakeRuntimeVarHandlerRepo) DeleteRuntimeVar(ctx context.Context, projectID, varSetID, key string) error {
	delete(f.vars[f.key(projectID, varSetID)], key)
	return nil
}

func (f *fakeRuntimeVarHandlerRepo) InsertVersion(ctx context.Context, v *projects.RuntimeVarVersion) error {
	cp := *v
	f.lastVersion = &cp
	return nil
}

func (f *fakeRuntimeVarHandlerRepo) BumpRevisionAndPrune(ctx context.Context, projectID, varSetID string) (int64, error) {
	k := f.key(projectID, varSetID)
	f.heads[k]++
	return f.heads[k], nil
}

// runtimeVarHandlerEnv 组装 handler + fake + admin Principal ctx。
type runtimeVarHandlerEnv struct {
	svc   *RuntimeVarsService
	repo  *fakeRuntimeVarHandlerRepo
	ctx   context.Context // admin principal + audit holder
	plain context.Context // 无 principal（Unauthenticated 用例）
}

func newRuntimeVarHandlerEnv(t *testing.T) *runtimeVarHandlerEnv {
	t.Helper()
	repo := newFakeRuntimeVarHandlerRepo()
	svc := NewRuntimeVarsService(appserver.NewRuntimeVars(repo, stubRuntimeVarRunner{}))
	principal := &shared.Principal{
		ActorKind: shared.ActorKindAdmin,
		AdminID:   "admin1",
		ProjectID: "p1",
	}
	return &runtimeVarHandlerEnv{
		svc:   svc,
		repo:  repo,
		ctx:   contexts.WithPrincipal(context.Background(), principal),
		plain: context.Background(),
	}
}

type stubRuntimeVarRunner struct{}

func (stubRuntimeVarRunner) Run(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// --- 集合：映射 + audit + 错误透传 ---

func TestRuntimeVarsHandlerVarSetMappingAndAudit(t *testing.T) {
	t.Parallel()
	env := newRuntimeVarHandlerEnv(t)

	// audit holder 预置（模拟 audit 拦截器）；CreateVarSet 标注集合资源。
	ctx := contexts.WithAuditResourceHolder(env.ctx)
	resp, err := env.svc.CreateVarSet(ctx, &serverv1.CreateVarSetRequest{
		VarSetId:    "flags",
		Description: "feature flags",
	})
	require.NoError(t, err)
	require.Equal(t, "flags", resp.GetVarSetId())
	require.Equal(t, serverv1.VarSetVisibility_VAR_SET_VISIBILITY_PUBLIC, resp.GetVisibility(), "UNSPECIFIED 归一为 public")
	require.Equal(t, "feature flags", resp.GetDescription())
	require.Contains(t, resp.GetEtag(), ":0", "创建即 revision 0 的 etag")
	require.Zero(t, resp.GetRevision())
	require.NotNil(t, resp.GetCreatedAt())
	require.Equal(t, "runtime_var_sets/flags", contexts.AuditResource(ctx))

	// private 显式声明 roundtrip。
	_, err = env.svc.CreateVarSet(env.ctx, &serverv1.CreateVarSetRequest{
		VarSetId:   "secret",
		Visibility: serverv1.VarSetVisibility_VAR_SET_VISIBILITY_PRIVATE,
	})
	require.NoError(t, err)
	got, err := env.svc.GetVarSet(env.ctx, &serverv1.GetVarSetRequest{VarSetId: "secret"})
	require.NoError(t, err)
	require.Equal(t, serverv1.VarSetVisibility_VAR_SET_VISIBILITY_PRIVATE, got.GetVisibility())

	// UpdateVarSet optional：只带 visibility。
	privVis := serverv1.VarSetVisibility_VAR_SET_VISIBILITY_PRIVATE
	updated, err := env.svc.UpdateVarSet(env.ctx, &serverv1.UpdateVarSetRequest{
		VarSetId:   "flags",
		Visibility: &privVis,
	})
	require.NoError(t, err)
	require.Equal(t, serverv1.VarSetVisibility_VAR_SET_VISIBILITY_PRIVATE, updated.GetVisibility())
	require.Equal(t, "feature flags", updated.GetDescription(), "未设置 description = 不修改")

	// ListVarSets 分页：2 集合默认页全返回。
	list, err := env.svc.ListVarSets(env.ctx, &sharedv1.ListRequest{})
	require.NoError(t, err)
	require.Len(t, list.GetVarSets(), 2)
	require.Equal(t, int32(2), list.GetMeta().GetTotalCount())

	// 错误透传：缺失 → NotFound（app 层语义直达）。
	_, err = env.svc.GetVarSet(env.ctx, &serverv1.GetVarSetRequest{VarSetId: "ghost"})
	require.Equal(t, codes.NotFound, status.Code(err))

	// 无项目上下文 → Unauthenticated。
	_, err = env.svc.GetVarSet(env.plain, &serverv1.GetVarSetRequest{VarSetId: "flags"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// --- 变量：oneof 映射 roundtrip + actor 派生 + var 级 audit ---

func TestRuntimeVarsHandlerVarMappingActorAndAudit(t *testing.T) {
	t.Parallel()
	env := newRuntimeVarHandlerEnv(t)

	_, err := env.svc.CreateVarSet(env.ctx, &serverv1.CreateVarSetRequest{VarSetId: "flags"})
	require.NoError(t, err)

	// 五类型 roundtrip（json 深层值原样文本）。
	for _, spec := range []struct {
		key   string
		value *serverv1.RuntimeVarValue
	}{
		{"s", &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_StringValue{StringValue: "hello"}}},
		{"i", &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_IntegerValue{IntegerValue: -42}}},
		{"f", &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_FloatValue{FloatValue: 3.14}}},
		{"b", &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_BoolValue{BoolValue: true}}},
		{"j", &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_JsonValue{JsonValue: `{"a": [1, null]}`}}},
	} {
		created, err := env.svc.CreateRuntimeVar(env.ctx, &serverv1.CreateRuntimeVarRequest{
			VarSetId: "flags", Key: spec.key, Value: spec.value, Description: "d-" + spec.key,
		})
		require.NoErrorf(t, err, "create %s", spec.key)
		require.Equal(t, spec.value.GetStringValue(), created.GetValue().GetStringValue())
		require.Equal(t, spec.value.GetIntegerValue(), created.GetValue().GetIntegerValue())
		require.InDelta(t, spec.value.GetFloatValue(), created.GetValue().GetFloatValue(), 1e-9)
		require.Equal(t, spec.value.GetBoolValue(), created.GetValue().GetBoolValue())
		require.Equal(t, spec.value.GetJsonValue(), created.GetValue().GetJsonValue())
		require.Equal(t, "flags", created.GetVarSetId())
		require.Equal(t, spec.key, created.GetKey())
		require.Equal(t, "d-"+spec.key, created.GetDescription())
	}

	// actor 前缀从 Principal 派生（admin 会话）。
	require.NotNil(t, env.repo.lastVersion)
	require.Equal(t, "admin:admin1", env.repo.lastVersion.Actor)

	// var 级 audit 资源：runtime_var_sets/<id>/vars/<key>。
	ctx := contexts.WithAuditResourceHolder(env.ctx)
	_, err = env.svc.GetRuntimeVar(ctx, &serverv1.GetRuntimeVarRequest{VarSetId: "flags", Key: "s"})
	require.NoError(t, err)
	require.Equal(t, "runtime_var_sets/flags/vars/s", contexts.AuditResource(ctx))

	// UpdateRuntimeVar：新值 + 类型切换；不带 description 保留原值。
	updated, err := env.svc.UpdateRuntimeVar(env.ctx, &serverv1.UpdateRuntimeVarRequest{
		VarSetId: "flags",
		Key:      "i",
		Value:    &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_StringValue{StringValue: "now-string"}},
	})
	require.NoError(t, err)
	require.Equal(t, "now-string", updated.GetValue().GetStringValue())
	require.Equal(t, "d-i", updated.GetDescription())

	// ListRuntimeVars。
	listed, err := env.svc.ListRuntimeVars(env.ctx, &serverv1.ListRuntimeVarsRequest{VarSetId: "flags"})
	require.NoError(t, err)
	require.Len(t, listed.GetVars(), 5)
	require.Equal(t, "b", listed.GetVars()[0].GetKey(), "key ASC")

	// 非法 JSON → InvalidArgument（值构造在 app 层，错误透传）。
	_, err = env.svc.CreateRuntimeVar(env.ctx, &serverv1.CreateRuntimeVarRequest{
		VarSetId: "flags",
		Key:      "bad",
		Value:    &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_JsonValue{JsonValue: `{"a":`}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// apikey actor 前缀。
	keyPrincipal := &shared.Principal{
		ActorKind: shared.ActorKindService,
		APIKeyID:  "key9",
		ProjectID: "p1",
	}
	_, err = env.svc.DeleteRuntimeVar(contexts.WithPrincipal(context.Background(), keyPrincipal),
		&serverv1.DeleteRuntimeVarRequest{VarSetId: "flags", Key: "b"})
	require.NoError(t, err)
	require.Equal(t, "apikey:key9", env.repo.lastVersion.Actor)
}

// --- 版本面：Rollback 映射 ---

func TestRuntimeVarsHandlerRollback(t *testing.T) {
	t.Parallel()
	env := newRuntimeVarHandlerEnv(t)

	_, err := env.svc.CreateVarSet(env.ctx, &serverv1.CreateVarSetRequest{VarSetId: "flags"})
	require.NoError(t, err)
	_, err = env.svc.CreateRuntimeVar(env.ctx, &serverv1.CreateRuntimeVarRequest{
		VarSetId: "flags", Key: "a",
		Value: &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_IntegerValue{IntegerValue: 1}},
	})
	require.NoError(t, err)

	// 回滚到当前 → InvalidArgument 透传（D12）。
	_, err = env.svc.RollbackRuntimeVar(env.ctx, &serverv1.RollbackRuntimeVarRequest{VarSetId: "flags", TargetRevision: 1})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// --- 纯映射函数 ---

func TestRuntimeVarValueFromStoredRoundtrip(t *testing.T) {
	t.Parallel()
	// string（JSON 文本带引号）。
	v, err := runtimeVarValueFromStored(projects.RuntimeVarTypeString, `"he\"llo"`)
	require.NoError(t, err)
	require.Equal(t, "he\"llo", v.GetStringValue())
	// json 原样。
	v, err = runtimeVarValueFromStored(projects.RuntimeVarTypeJSON, `{"a":1}`)
	require.NoError(t, err)
	require.Equal(t, `{"a":1}`, v.GetJsonValue())
	// 损坏数据 → Internal。
	_, err = runtimeVarValueFromStored(projects.RuntimeVarTypeInteger, `not-a-number`)
	require.Equal(t, codes.Internal, status.Code(err))
	_, err = runtimeVarValueFromStored("unknown", `x`)
	require.Equal(t, codes.Internal, status.Code(err))
}
