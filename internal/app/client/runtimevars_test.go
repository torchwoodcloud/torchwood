package client

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rtVarsProjectRepo：RuntimeVars 用例测试的项目仓储桩（未知项目返回 nil）。
type rtVarsProjectRepo struct {
	project *projects.Project
	err     error
}

func (r *rtVarsProjectRepo) CreateProject(context.Context, *projects.Project) error { return nil }
func (r *rtVarsProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.project != nil && r.project.ID == id {
		return r.project, nil
	}
	return nil, nil
}
func (r *rtVarsProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (r *rtVarsProjectRepo) ListProjects(context.Context) ([]projects.Project, error) {
	return nil, nil
}
func (r *rtVarsProjectRepo) UpdateProject(context.Context, *projects.Project) error { return nil }
func (r *rtVarsProjectRepo) DeleteProject(context.Context, string) error            { return nil }
func (r *rtVarsProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error {
	return nil
}

// rtVarsFakeRead：RuntimeVarPublicRead 桩。visible=false 模拟 port 内合并的
// 三种零返回（private 未授权 / 集合不存在）；allowedIn 记录用例层传入的
// 凭证布尔（可见性过滤语义在 port、凭证布尔在用例层，两处分别断言，§6）。
type rtVarsFakeRead struct {
	visible   bool
	vars      []projects.RuntimeVar
	epoch     string
	revision  int64
	err       error
	allowedIn []bool
}

func (f *rtVarsFakeRead) GetVisibleVars(_ context.Context, _, _ string, principalAllowed bool) ([]projects.RuntimeVar, string, int64, error) {
	f.allowedIn = append(f.allowedIn, principalAllowed)
	if f.err != nil {
		return nil, "", 0, f.err
	}
	if !f.visible {
		return nil, "", 0, nil
	}
	return f.vars, f.epoch, f.revision, nil
}

func newRuntimeVarsForTest(repo *rtVarsProjectRepo, read *rtVarsFakeRead) *RuntimeVars {
	return NewRuntimeVars(repo, read)
}

func rtVarsPrincipal(projectID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		UserID:    "u1",
		ProjectID: projectID,
	})
}

// TestClientRuntimeVars_VisibilityMatrix 覆盖 §2.4 可见性矩阵全格：凭证布尔
// 在用例层（allowedIn 断言）、可见性过滤语义在 port（visible 开关）。
func TestClientRuntimeVars_VisibilityMatrix(t *testing.T) {
	vars := []projects.RuntimeVar{{Key: "flag", ValueType: "boolean", Value: "true"}}

	cases := []struct {
		name         string
		principal    bool // ctx 是否注入 Principal
		principalPID string
		visible      bool // port 是否返回快照（public/授权 private）还是零返回（private 未授权）
		wantCode     codes.Code
		wantAllowed  bool
	}{
		{name: "匿名×public集=200", visible: true, wantCode: codes.OK, wantAllowed: false},
		{name: "匿名×private集=NotFound", visible: false, wantCode: codes.NotFound, wantAllowed: false},
		{name: "登录×private集=200", principal: true, principalPID: "proj-1", visible: true, wantCode: codes.OK, wantAllowed: true},
		{name: "登录×public集=200", principal: true, principalPID: "proj-1", visible: true, wantCode: codes.OK, wantAllowed: true},
		{name: "跨项目Principal=PermissionDenied", principal: true, principalPID: "proj-2", visible: true, wantCode: codes.PermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := &rtVarsFakeRead{visible: tc.visible, vars: vars, epoch: "a1b2c3d4", revision: 3}
			uc := newRuntimeVarsForTest(&rtVarsProjectRepo{project: &projects.Project{ID: "proj-1"}}, read)
			ctx := context.Background()
			if tc.principal {
				ctx = rtVarsPrincipal(tc.principalPID)
			}
			snap, err := uc.GetRuntimeVars(ctx, "proj-1", "features", "")
			if tc.wantCode == codes.OK {
				require.NoError(t, err)
				require.False(t, snap.Unchanged)
				require.Equal(t, vars, snap.Vars)
				require.Equal(t, "a1b2c3d4:3", snap.ETag)
			} else {
				require.Nil(t, snap)
				require.Equal(t, tc.wantCode, status.Code(err))
			}
			if tc.wantCode != codes.PermissionDenied {
				require.Equal(t, []bool{tc.wantAllowed}, read.allowedIn)
			} else {
				// 跨项目在凭证布尔处拒绝，port 不被调用。
				require.Empty(t, read.allowedIn)
			}
		})
	}
}

// TestClientRuntimeVars_ETag：命中短路（vars 空）/未命中全量/epoch 变化必全量（D2）。
func TestClientRuntimeVars_ETag(t *testing.T) {
	newUC := func(revision int64) (*RuntimeVars, *rtVarsFakeRead) {
		read := &rtVarsFakeRead{
			visible:  true,
			vars:     []projects.RuntimeVar{{Key: "flag", ValueType: "boolean", Value: "true"}},
			epoch:    "e2etest01",
			revision: revision,
		}
		return newRuntimeVarsForTest(&rtVarsProjectRepo{project: &projects.Project{ID: "proj-1"}}, read), read
	}

	t.Run("etag命中→unchanged且vars空", func(t *testing.T) {
		uc, read := newUC(3)
		snap, err := uc.GetRuntimeVars(context.Background(), "proj-1", "features", "e2etest01:3")
		require.NoError(t, err)
		require.True(t, snap.Unchanged)
		require.Nil(t, snap.Vars)
		require.Equal(t, "e2etest01:3", snap.ETag)
		require.Len(t, read.allowedIn, 1) // port 被调用（unchanged 判定在 port 读之后）
	})

	t.Run("etag未命中→全量+新etag", func(t *testing.T) {
		uc, _ := newUC(4)
		snap, err := uc.GetRuntimeVars(context.Background(), "proj-1", "features", "e2etest01:3")
		require.NoError(t, err)
		require.False(t, snap.Unchanged)
		require.Len(t, snap.Vars, 1)
		require.Equal(t, "e2etest01:4", snap.ETag)
	})

	t.Run("epoch变化必全量（同名重建碰撞，D2）", func(t *testing.T) {
		uc, _ := newUC(3) // revision 相同但 epoch 已变
		snap, err := uc.GetRuntimeVars(context.Background(), "proj-1", "features", "oldepoch:3")
		require.NoError(t, err)
		require.False(t, snap.Unchanged)
		require.Len(t, snap.Vars, 1)
		require.Equal(t, "e2etest01:3", snap.ETag)
	})

	t.Run("空etag→全量", func(t *testing.T) {
		uc, _ := newUC(0)
		snap, err := uc.GetRuntimeVars(context.Background(), "proj-1", "features", "")
		require.NoError(t, err)
		require.False(t, snap.Unchanged)
		require.Len(t, snap.Vars, 1)
		require.Equal(t, "e2etest01:0", snap.ETag)
	})
}

// TestClientRuntimeVars_ProjectErrors：缺/未知 project_id → InvalidArgument
// （匿名端点不确认项目存在性）；仓储错误透传；port 错误透传。
func TestClientRuntimeVars_ProjectErrors(t *testing.T) {
	t.Run("缺project_id→InvalidArgument", func(t *testing.T) {
		uc := newRuntimeVarsForTest(&rtVarsProjectRepo{project: &projects.Project{ID: "proj-1"}}, &rtVarsFakeRead{})
		_, err := uc.GetRuntimeVars(context.Background(), "", "features", "")
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "project_id is required")
	})

	t.Run("未知project→InvalidArgument", func(t *testing.T) {
		uc := newRuntimeVarsForTest(&rtVarsProjectRepo{project: &projects.Project{ID: "proj-1"}}, &rtVarsFakeRead{})
		_, err := uc.GetRuntimeVars(context.Background(), "proj-unknown", "features", "")
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("仓储错误透传", func(t *testing.T) {
		uc := newRuntimeVarsForTest(&rtVarsProjectRepo{err: errors.New("boom")}, &rtVarsFakeRead{})
		_, err := uc.GetRuntimeVars(context.Background(), "proj-1", "features", "")
		require.EqualError(t, err, "boom")
	})

	t.Run("port错误透传", func(t *testing.T) {
		uc := newRuntimeVarsForTest(&rtVarsProjectRepo{project: &projects.Project{ID: "proj-1"}}, &rtVarsFakeRead{err: errors.New("db down")})
		_, err := uc.GetRuntimeVars(context.Background(), "proj-1", "features", "")
		require.EqualError(t, err, "db down")
	})
}

// TestClientRuntimeVars_NilVarsNotFound：port 零返回 → NotFound，且错误体与
// "集合不存在"完全一致（private 与不存在不可区分，§2.4）。
func TestClientRuntimeVars_NilVarsNotFound(t *testing.T) {
	uc := newRuntimeVarsForTest(&rtVarsProjectRepo{project: &projects.Project{ID: "proj-1"}}, &rtVarsFakeRead{visible: false})
	_, err := uc.GetRuntimeVars(context.Background(), "proj-1", "private_set", "")
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Equal(t, "rpc error: code = NotFound desc = var set not found", err.Error())

	// 同项目下真正不存在的集名——错误体逐字一致。
	_, errMissing := uc.GetRuntimeVars(context.Background(), "proj-1", "definitely_missing", "")
	require.Equal(t, err.Error(), errMissing.Error())
}
