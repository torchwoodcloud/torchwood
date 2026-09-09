package clientgrpc

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FunctionsService 是客户端调用面 gRPC handler（P2，设计 §4）。
type FunctionsService struct {
	clientv1.UnimplementedFunctionsServiceServer
	functions *appfunctions.Functions
}

// NewFunctionsService constructs the client functions service.
func NewFunctionsService(functions *appfunctions.Functions) *FunctionsService {
	return &FunctionsService{functions: functions}
}

// InvokeFunction 同步调用一个 client_callable 函数（端用户身份取自
// Principal——请求体不携带身份；project/user 均由平台注入）。
func (s *FunctionsService) InvokeFunction(ctx context.Context, req *clientv1.InvokeFunctionRequest) (*clientv1.InvokeFunctionResponse, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil || p.ProjectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	res, err := s.functions.ClientInvoke(ctx, appfunctions.ClientInvokeCommand{
		ProjectID:      p.ProjectID,
		FunctionID:     req.GetFunctionId(),
		Data:           req.GetData(),
		DeploymentID:   req.GetDeploymentId(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, err
	}
	return mapClientInvokeResponse(res), nil
}

func mapClientInvokeResponse(res *appfunctions.ClientInvokeResult) *clientv1.InvokeFunctionResponse {
	rec := res.Record
	// 终态 failed 语义保持原样返回（HTTP 200 + status=failed）：函数执行
	// 失败是结果而非传输错误，容器 exit code 对客户端无语义（设计 §4）。
	// status 透传执行记录状态（completed | failed | running）。
	return &clientv1.InvokeFunctionResponse{
		ExecutionId: rec.ID,
		Status:      rec.Status,
		Response:    rec.Response,
	}
}
