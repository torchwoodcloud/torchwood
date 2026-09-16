package clientgrpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/genproto/client/v1"
	appclient "github.com/torchwoodcloud/torchwood/internal/app/client"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

// rtVarsHandlerRead：clientgrpc 层测试用的 RuntimeVarPublicRead 桩。
type rtVarsHandlerRead struct {
	vars     []projects.RuntimeVar
	epoch    string
	revision int64
}

func (f *rtVarsHandlerRead) GetVisibleVars(_ context.Context, _, _ string, _ bool) ([]projects.RuntimeVar, string, int64, error) {
	return f.vars, f.epoch, f.revision, nil
}

func newRuntimeVarsHandler(t *testing.T, projectID string, read *rtVarsHandlerRead) *RuntimeVarsService {
	t.Helper()
	return NewRuntimeVarsService(appclient.NewRuntimeVars(
		&fakeProjectRepo{project: &projects.Project{ID: projectID}}, read))
}

// rtVarsMultiProjectRepo：支持多项目的仓储桩（三级回落链用——各级指向的
// 项目都必须真实存在，才能区分"回落到谁"与"项目不存在"）。
type rtVarsMultiProjectRepo struct {
	ids map[string]bool
}

func (r *rtVarsMultiProjectRepo) CreateProject(context.Context, *projects.Project) error { return nil }
func (r *rtVarsMultiProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	if r.ids[id] {
		return &projects.Project{ID: id}, nil
	}
	return nil, nil
}
func (r *rtVarsMultiProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (r *rtVarsMultiProjectRepo) ListProjects(context.Context) ([]projects.Project, error) {
	return nil, nil
}
func (r *rtVarsMultiProjectRepo) UpdateProject(context.Context, *projects.Project) error { return nil }
func (r *rtVarsMultiProjectRepo) DeleteProject(context.Context, string) error            { return nil }
func (r *rtVarsMultiProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error {
	return nil
}

// TestClientGRPC_GetRuntimeVars_ResolveProjectID：三级回落链（请求字段 →
// X-Torchwood-Project header → Principal）与全空 InvalidArgument。
func TestClientGRPC_GetRuntimeVars_ResolveProjectID(t *testing.T) {
	read := &rtVarsHandlerRead{vars: []projects.RuntimeVar{{Key: "flag", ValueType: "boolean", Value: "true"}}, epoch: "e2e", revision: 1}
	multi := func(ids ...string) *rtVarsMultiProjectRepo {
		m := &rtVarsMultiProjectRepo{ids: map[string]bool{}}
		for _, id := range ids {
			m.ids[id] = true
		}
		return m
	}

	t.Run("请求字段优先于header", func(t *testing.T) {
		// proj-header 不存在：若回落 header 必 InvalidArgument，NoError 即
		// 证明请求字段胜出。
		svc := NewRuntimeVarsService(appclient.NewRuntimeVars(multi("proj-req"), read))
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-torchwood-project", "proj-header"))
		resp, err := svc.GetRuntimeVars(ctx, &clientv1.GetRuntimeVarsRequest{VarSetId: "features", ProjectId: "proj-req"})
		require.NoError(t, err)
		require.Equal(t, "e2e:1", resp.GetEtag())
	})

	t.Run("请求字段优先于Principal（跨项目→PermissionDenied）", func(t *testing.T) {
		// 请求字段 proj-req 存在；Principal 属 proj-principal。若回落
		// Principal 会 InvalidArgument（proj-principal 不在 repo）；得到
		// PermissionDenied 恰证明寻址用了请求字段、凭证布尔随后拒绝
		//（§2.4 矩阵第三行）。
		svc := NewRuntimeVarsService(appclient.NewRuntimeVars(multi("proj-req", "proj-principal"), read))
		ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
			ActorKind: shared.ActorKindEndUser, UserID: "u1", ProjectID: "proj-principal",
		})
		_, err := svc.GetRuntimeVars(ctx, &clientv1.GetRuntimeVarsRequest{VarSetId: "features", ProjectId: "proj-req"})
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("回落header", func(t *testing.T) {
		svc := NewRuntimeVarsService(appclient.NewRuntimeVars(multi("proj-header"), read))
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("X-Torchwood-Project", "proj-header"))
		resp, err := svc.GetRuntimeVars(ctx, &clientv1.GetRuntimeVarsRequest{VarSetId: "features"})
		require.NoError(t, err)
		require.Equal(t, "e2e:1", resp.GetEtag())
	})

	t.Run("回落Principal", func(t *testing.T) {
		svc := NewRuntimeVarsService(appclient.NewRuntimeVars(multi("proj-principal"), read))
		ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
			ActorKind: shared.ActorKindEndUser, UserID: "u1", ProjectID: "proj-principal",
		})
		resp, err := svc.GetRuntimeVars(ctx, &clientv1.GetRuntimeVarsRequest{VarSetId: "features"})
		require.NoError(t, err)
		require.Equal(t, "e2e:1", resp.GetEtag())
	})

	t.Run("全空→InvalidArgument", func(t *testing.T) {
		svc := NewRuntimeVarsService(appclient.NewRuntimeVars(multi("proj-1"), read))
		_, err := svc.GetRuntimeVars(context.Background(), &clientv1.GetRuntimeVarsRequest{VarSetId: "features"})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "project_id is required")
	})
}

// TestClientGRPC_GetRuntimeVars_ValueMapping：五类型值映射 + etag 命中短路
// （unchanged 时 vars 空 map）。
func TestClientGRPC_GetRuntimeVars_ValueMapping(t *testing.T) {
	read := &rtVarsHandlerRead{
		vars: []projects.RuntimeVar{
			{Key: "s_val", ValueType: "string", Value: `"hello"`},
			{Key: "i_val", ValueType: "integer", Value: `42`},
			{Key: "f_val", ValueType: "float", Value: `3.5`},
			{Key: "b_val", ValueType: "boolean", Value: `true`},
			{Key: "j_val", ValueType: "json", Value: `{"a":1}`},
		},
		epoch:    "aabbccdd",
		revision: 7,
	}
	svc := newRuntimeVarsHandler(t, "proj-1", read)

	resp, err := svc.GetRuntimeVars(context.Background(), &clientv1.GetRuntimeVarsRequest{VarSetId: "features", ProjectId: "proj-1"})
	require.NoError(t, err)
	require.Equal(t, "aabbccdd:7", resp.GetEtag())
	require.False(t, resp.GetUnchanged())
	require.Len(t, resp.GetVars(), 5)
	require.Equal(t, "hello", resp.GetVars()["s_val"].GetStringValue())
	require.Equal(t, int64(42), resp.GetVars()["i_val"].GetIntegerValue())
	require.Equal(t, 3.5, resp.GetVars()["f_val"].GetFloatValue())
	require.True(t, resp.GetVars()["b_val"].GetBoolValue())
	require.Equal(t, `{"a":1}`, resp.GetVars()["j_val"].GetJsonValue())

	// etag 命中 → unchanged=true 且 vars 为空（不再做值映射）。
	resp2, err := svc.GetRuntimeVars(context.Background(), &clientv1.GetRuntimeVarsRequest{
		VarSetId: "features", ProjectId: "proj-1", Etag: "aabbccdd:7",
	})
	require.NoError(t, err)
	require.True(t, resp2.GetUnchanged())
	require.Empty(t, resp2.GetVars())
	require.Equal(t, "aabbccdd:7", resp2.GetEtag())
}

// TestClientGRPC_GetRuntimeVars_NotFound：port 零返回（private 未授权/集合
// 不存在）→ NotFound 透传。
func TestClientGRPC_GetRuntimeVars_NotFound(t *testing.T) {
	empty := &rtVarsHandlerRead{} // vars nil → 用例层映射 NotFound
	svc := NewRuntimeVarsService(appclient.NewRuntimeVars(
		&fakeProjectRepo{project: &projects.Project{ID: "proj-1"}}, empty))
	_, err := svc.GetRuntimeVars(context.Background(), &clientv1.GetRuntimeVarsRequest{VarSetId: "private_set", ProjectId: "proj-1"})
	require.Equal(t, codes.NotFound, status.Code(err))
}
