package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// FunctionsService 封装 Server API 的 Functions 服务（v4 面：target + 调用
// + 触发器；部署/变量/运行时 RPC 随执行面外包退役，docs/design/functions-v4.md §2）。
type FunctionsService struct {
	c   *Client
	api serverv1.FunctionsServiceClient
}

// CreateFunction 创建函数（endpoint target + 签名密钥明文一次性回显）。
func (s *FunctionsService) CreateFunction(ctx context.Context, req *serverv1.CreateFunctionRequest) (*serverv1.CreateFunctionResponse, error) {
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

// SetFunctionScopes 全量替换函数 declared_scopes（执行身份，P0）。
func (s *FunctionsService) SetFunctionScopes(ctx context.Context, req *serverv1.SetFunctionScopesRequest) (*serverv1.Function, error) {
	return s.api.SetFunctionScopes(ctx, req)
}

// RotateFunctionSecret 轮换 endpoint target 签名密钥（新明文一次性回显；
// 旧钥保留一个宽限窗双签 v2）。
func (s *FunctionsService) RotateFunctionSecret(ctx context.Context, req *serverv1.RotateFunctionSecretRequest) (*serverv1.FunctionSecret, error) {
	return s.api.RotateFunctionSecret(ctx, req)
}

// InvokeFunction 调用函数（同步或 async；v4 §2 由 CreateExecution 改名）。
func (s *FunctionsService) InvokeFunction(ctx context.Context, req *serverv1.InvokeFunctionRequest) (*serverv1.Invocation, error) {
	return s.api.InvokeFunction(ctx, req)
}

// ListInvocations 分页列出调用记录（支持 status/created_at 过滤）。
func (s *FunctionsService) ListInvocations(ctx context.Context, req *serverv1.ListInvocationsRequest) (*serverv1.ListInvocationsResponse, error) {
	return s.api.ListInvocations(ctx, req)
}

// GetInvocation 按 ID 获取调用记录。
func (s *FunctionsService) GetInvocation(ctx context.Context, req *serverv1.GetInvocationRequest) (*serverv1.Invocation, error) {
	return s.api.GetInvocation(ctx, req)
}

// CreateFunctionTrigger 创建函数触发器：type=http 需带 http 配置段（token
// 服务端生成）；type=cron 需带 cron 配置段；type=event 需带 event 配置段。
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
