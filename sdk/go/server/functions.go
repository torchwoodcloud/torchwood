package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// FunctionsService 封装 Server API 的 Functions 服务。
type FunctionsService struct {
	c   *Client
	api serverv1.FunctionsServiceClient
}

// ListRuntimes 列出支持的运行时。
func (s *FunctionsService) ListRuntimes(ctx context.Context, req *sharedv1.Empty) (*serverv1.ListRuntimesResponse, error) {
	return s.api.ListRuntimes(ctx, req)
}

// ListSpecifications 列出支持的资源配置。
func (s *FunctionsService) ListSpecifications(ctx context.Context, req *sharedv1.Empty) (*serverv1.ListSpecificationsResponse, error) {
	return s.api.ListSpecifications(ctx, req)
}

// CreateFunction 创建函数。
func (s *FunctionsService) CreateFunction(ctx context.Context, req *serverv1.CreateFunctionRequest) (*serverv1.Function, error) {
	return s.api.CreateFunction(ctx, req)
}

// ListFunctions 列出函数。
func (s *FunctionsService) ListFunctions(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListFunctionsResponse, error) {
	return s.api.ListFunctions(ctx, req)
}

// GetFunction 按 ID 获取函数。
func (s *FunctionsService) GetFunction(ctx context.Context, req *serverv1.GetFunctionRequest) (*serverv1.Function, error) {
	return s.api.GetFunction(ctx, req)
}

// UpdateFunction 更新函数（仅更新显式传入的字段）。
func (s *FunctionsService) UpdateFunction(ctx context.Context, req *serverv1.UpdateFunctionRequest) (*serverv1.Function, error) {
	return s.api.UpdateFunction(ctx, req)
}

// DeleteFunction 删除函数。
func (s *FunctionsService) DeleteFunction(ctx context.Context, req *serverv1.GetFunctionRequest) error {
	_, err := s.api.DeleteFunction(ctx, req)
	return err
}

// CreateDeployment 上传 zip 代码包创建部署。
func (s *FunctionsService) CreateDeployment(ctx context.Context, req *serverv1.CreateDeploymentRequest) (*serverv1.Deployment, error) {
	return s.api.CreateDeployment(ctx, req)
}

// ListDeployments 列出函数部署。
func (s *FunctionsService) ListDeployments(ctx context.Context, req *serverv1.GetFunctionRequest) (*serverv1.ListDeploymentsResponse, error) {
	return s.api.ListDeployments(ctx, req)
}

// GetDeployment 按 ID 获取部署。
func (s *FunctionsService) GetDeployment(ctx context.Context, req *serverv1.GetDeploymentRequest) (*serverv1.Deployment, error) {
	return s.api.GetDeployment(ctx, req)
}

// DeleteDeployment 删除部署。
func (s *FunctionsService) DeleteDeployment(ctx context.Context, req *serverv1.GetDeploymentRequest) error {
	_, err := s.api.DeleteDeployment(ctx, req)
	return err
}

// SetVariables 全量替换函数环境变量。
func (s *FunctionsService) SetVariables(ctx context.Context, req *serverv1.SetVariablesRequest) (*serverv1.Variables, error) {
	return s.api.SetVariables(ctx, req)
}

// GetVariables 获取函数环境变量。
func (s *FunctionsService) GetVariables(ctx context.Context, req *serverv1.GetFunctionRequest) (*serverv1.Variables, error) {
	return s.api.GetVariables(ctx, req)
}

// SetFunctionScopes 全量替换函数 declared_scopes（执行身份，P0）。
func (s *FunctionsService) SetFunctionScopes(ctx context.Context, req *serverv1.SetFunctionScopesRequest) (*serverv1.Function, error) {
	return s.api.SetFunctionScopes(ctx, req)
}

// CreateFunctionTrigger 创建函数触发器（P1）：type=http 需带 http 配置段
// （token 服务端生成）；type=cron 需带 cron 配置段。
func (s *FunctionsService) CreateFunctionTrigger(ctx context.Context, req *serverv1.CreateFunctionTriggerRequest) (*serverv1.FunctionTrigger, error) {
	return s.api.CreateFunctionTrigger(ctx, req)
}

// ListFunctionTriggers 列出函数触发器。
func (s *FunctionsService) ListFunctionTriggers(ctx context.Context, req *serverv1.GetFunctionRequest) (*serverv1.ListFunctionTriggersResponse, error) {
	return s.api.ListFunctionTriggers(ctx, req)
}

// DeleteFunctionTrigger 删除函数触发器。
func (s *FunctionsService) DeleteFunctionTrigger(ctx context.Context, req *serverv1.DeleteFunctionTriggerRequest) error {
	_, err := s.api.DeleteFunctionTrigger(ctx, req)
	return err
}

// RotateFunctionTriggerToken 轮换 http 触发器 token（旧 token 立即失效）。
func (s *FunctionsService) RotateFunctionTriggerToken(ctx context.Context, req *serverv1.RotateFunctionTriggerTokenRequest) (*serverv1.FunctionTrigger, error) {
	return s.api.RotateFunctionTriggerToken(ctx, req)
}

// CreateExecution 创建函数执行（同步或异步）。
func (s *FunctionsService) CreateExecution(ctx context.Context, req *serverv1.CreateExecutionRequest) (*serverv1.Execution, error) {
	return s.api.CreateExecution(ctx, req)
}

// ListExecutions 列出执行记录。
func (s *FunctionsService) ListExecutions(ctx context.Context, req *serverv1.GetFunctionRequest) (*serverv1.ListExecutionsResponse, error) {
	return s.api.ListExecutions(ctx, req)
}

// GetExecution 按 ID 获取执行记录。
func (s *FunctionsService) GetExecution(ctx context.Context, req *serverv1.GetExecutionRequest) (*serverv1.Execution, error) {
	return s.api.GetExecution(ctx, req)
}
