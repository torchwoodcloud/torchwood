package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// RuntimeVarsService 封装 Server API 的运行时变量管理面（docs/design/
// runtime-vars.md §2.3）：集合 CRUD + 变量 CRUD + 版本链查看/回滚，三组
// 13 方法。项目上下文来自凭证（请求体不携带 project_id）；读动词 =
// runtime_vars:read（viewer 含），写动词 = runtime_vars.write（admin/owner）。
type RuntimeVarsService struct {
	c   *Client
	api serverv1.RuntimeVarsServiceClient
}

// CreateVarSet 创建变量集合（var_set_id 创建后不可改，D9；重命名 = 重建 +
// 回滚替代）。visibility 缺省由 app 层归一为 public。
func (s *RuntimeVarsService) CreateVarSet(ctx context.Context, req *serverv1.CreateVarSetRequest) (*serverv1.VarSet, error) {
	return s.api.CreateVarSet(ctx, req)
}

// ListVarSets 列出项目的变量集合。
func (s *RuntimeVarsService) ListVarSets(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListVarSetsResponse, error) {
	return s.api.ListVarSets(ctx, req)
}

// GetVarSet 获取变量集合（footprint：var_count/total_bytes 供 D13 限额展示）。
func (s *RuntimeVarsService) GetVarSet(ctx context.Context, req *serverv1.GetVarSetRequest) (*serverv1.VarSet, error) {
	return s.api.GetVarSet(ctx, req)
}

// UpdateVarSet 只改集合元数据（optional visibility/description，未设置 =
// 不修改）；元数据变更不 bump revision、不入版本链（D10），仅进审计。
func (s *RuntimeVarsService) UpdateVarSet(ctx context.Context, req *serverv1.UpdateVarSetRequest) (*serverv1.VarSet, error) {
	return s.api.UpdateVarSet(ctx, req)
}

// DeleteVarSet 删除集合；vars/heads/versions 经 FK CASCADE 连带销毁（全部
// 历史不可恢复，调用方需自行确认）。
func (s *RuntimeVarsService) DeleteVarSet(ctx context.Context, req *serverv1.DeleteVarSetRequest) error {
	_, err := s.api.DeleteVarSet(ctx, req)
	return err
}

// CreateRuntimeVar 在集合内创建类型化变量（类型创建时锁定，D3）。
func (s *RuntimeVarsService) CreateRuntimeVar(ctx context.Context, req *serverv1.CreateRuntimeVarRequest) (*serverv1.RuntimeVar, error) {
	return s.api.CreateRuntimeVar(ctx, req)
}

// ListRuntimeVars 列出集合内变量（page_size ≤ 1000）。
func (s *RuntimeVarsService) ListRuntimeVars(ctx context.Context, req *serverv1.ListRuntimeVarsRequest) (*serverv1.ListRuntimeVarsResponse, error) {
	return s.api.ListRuntimeVars(ctx, req)
}

// GetRuntimeVar 获取单个变量。
func (s *RuntimeVarsService) GetRuntimeVar(ctx context.Context, req *serverv1.GetRuntimeVarRequest) (*serverv1.RuntimeVar, error) {
	return s.api.GetRuntimeVar(ctx, req)
}

// UpdateRuntimeVar 携带完整新值（必填，无 Upsert，D5）；类型可随本次变更
// （value 与 value_type 同步落库）。optional description：未设置 = 不修改。
func (s *RuntimeVarsService) UpdateRuntimeVar(ctx context.Context, req *serverv1.UpdateRuntimeVarRequest) (*serverv1.RuntimeVar, error) {
	return s.api.UpdateRuntimeVar(ctx, req)
}

// DeleteRuntimeVar 删除单个变量。
func (s *RuntimeVarsService) DeleteRuntimeVar(ctx context.Context, req *serverv1.DeleteRuntimeVarRequest) error {
	_, err := s.api.DeleteRuntimeVar(ctx, req)
	return err
}

// ListRuntimeVarVersions 列出版本链元数据（仅 revision/action/summary/
// actor/时间；全文走 GetRuntimeVarVersion）。版本保留最近 50 版/集合（D11）。
func (s *RuntimeVarsService) ListRuntimeVarVersions(ctx context.Context, req *serverv1.ListRuntimeVarVersionsRequest) (*serverv1.ListRuntimeVarVersionsResponse, error) {
	return s.api.ListRuntimeVarVersions(ctx, req)
}

// GetRuntimeVarVersion 获取某版本的全量快照（key ASC）。
func (s *RuntimeVarsService) GetRuntimeVarVersion(ctx context.Context, req *serverv1.GetRuntimeVarVersionRequest) (*serverv1.GetRuntimeVarVersionResponse, error) {
	return s.api.GetRuntimeVarVersion(ctx, req)
}

// RollbackRuntimeVar 回滚到目标版本（D12：target_revision 等于当前版本 →
// InvalidArgument；目标已被窗口淘汰 → NotFound "target revision pruned"）。
func (s *RuntimeVarsService) RollbackRuntimeVar(ctx context.Context, req *serverv1.RollbackRuntimeVarRequest) (*serverv1.RuntimeVarVersion, error) {
	return s.api.RollbackRuntimeVar(ctx, req)
}
