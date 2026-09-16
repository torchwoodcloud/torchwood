package client

import (
	"context"
	"strconv"

	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RuntimeVars 是 client 面运行时变量拉取用例（docs/design/runtime-vars.md
// §2.4/§2.6）：凭证布尔在本层判定（有有效 Principal 且项目匹配），集合级
// 可见性过滤下推到 RuntimeVarPublicRead 实现（接口上不存在未过滤读，旁路
// 在类型层消灭）。不注入全量 RuntimeVarRepository——wire 误接线为编译错误。
type RuntimeVars struct {
	projectRepo projects.Repository
	publicRead  projects.RuntimeVarPublicRead
}

func NewRuntimeVars(projectRepo projects.Repository, publicRead projects.RuntimeVarPublicRead) *RuntimeVars {
	return &RuntimeVars{projectRepo: projectRepo, publicRead: publicRead}
}

// RuntimeVarsSnapshot 是 GetRuntimeVars 的结果投影：全量快照 + 不透明 etag
// （"{epoch}:{revision}"，D2）+ unchanged 短路标记。Unchanged=true 时 Vars
// 为 nil（vars 空返回，§2.3）。
type RuntimeVarsSnapshot struct {
	Vars      []projects.RuntimeVar
	ETag      string
	Unchanged bool
}

// GetRuntimeVars 拉取集合的可见全量快照（§2.4 可见性矩阵）：
//   - 缺/未知 project_id → InvalidArgument（匿名端点不确认项目存在性，
//     与"private 集与不存在集同答"同口径；区别于 client/databases 的
//     loadProject NotFound——那是登录态读路径）；
//   - 凭证布尔：有有效 Principal 且（ProjectID 为空或 == 请求项目）；
//     Principal.ProjectID ≠ "" 且 ≠ 请求项目 → PermissionDenied；
//   - port 返回 nil 集合（private 且未授权 / 集合不存在，port 内合并）→
//     NotFound（与"集合不存在"同错误体，不向匿名探测者确认私有集合存在）；
//   - 请求 etag 非空且与当前 "{epoch}:{revision}" 相等 → unchanged=true
//     且 vars 为空；否则全量 vars + 新 etag。
//
// epoch 变化（同名删除重建）必然 etag 不等 → 全量返回（D2 防碰撞）。
func (r *RuntimeVars) GetRuntimeVars(ctx context.Context, projectID, varSetID, etag string) (*RuntimeVarsSnapshot, error) {
	if projectID == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	project, err := r.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid project_id")
	}
	allowed, err := principalAllowedForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	vars, epoch, revision, err := r.publicRead.GetVisibleVars(ctx, projectID, varSetID, allowed)
	if err != nil {
		return nil, err
	}
	if vars == nil {
		return nil, status.Error(codes.NotFound, "var set not found")
	}
	// 与 server 面 VarSetView.ETag 同拼接（internal/app/server/runtimevars.go）。
	current := epoch + ":" + strconv.FormatInt(revision, 10)
	if etag != "" && etag == current {
		return &RuntimeVarsSnapshot{ETag: current, Unchanged: true}, nil
	}
	return &RuntimeVarsSnapshot{Vars: vars, ETag: current}, nil
}

// principalAllowedForProject 判定凭证布尔（§2.4 矩阵第一列）：匿名（无
// Principal / 未认证）→ false；有效 Principal（end_user / admin / apikey /
// 函数执行）且项目匹配 → true；跨项目 Principal（ProjectID ≠ "" 且 ≠ 请求
// 项目）→ PermissionDenied。
func principalAllowedForProject(ctx context.Context, projectID string) (bool, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil || !p.IsAuthenticated() {
		return false, nil
	}
	if p.ProjectID != "" && p.ProjectID != projectID {
		return false, status.Error(codes.PermissionDenied, "project_id mismatch")
	}
	return true, nil
}
