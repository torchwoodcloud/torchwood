package client

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
)

// FunctionsService 封装 Client API 的客户端调用面（P2）：终端用户按
// per-function 策略（client_callable + 每用户限频）同步调用函数。
type FunctionsService struct{ c *Client }

// InvokeFunction 同步调用一个 client_callable 函数（≤30s 沿用平台同步路径）。
//
//   - data 必须是 JSON object 字符串（≤32KB）；
//   - idempotencyKey 可选：网络超时重试防重复执行——(project, function, user,
//     key) 唯一去重，命中返回既有 execution 原样（进行中返回 running）；
//   - 配额超额返回 ResourceExhausted（ErrorInfo.Reason =
//     FUNCTIONS.INVOKE_QUOTA_EXCEEDED + RetryInfo 指向窗口结束时刻）；
//   - 函数执行失败不是传输错误：HTTP 200 + status="failed"。
func (f *FunctionsService) InvokeFunction(ctx context.Context, req *clientv1.InvokeFunctionRequest) (*clientv1.InvokeFunctionResponse, error) {
	return f.c.functions.InvokeFunction(ctx, req)
}

// InvokeString 用基本类型调用函数（deploymentID 传空 = 最新 ready）。
func (f *FunctionsService) InvokeString(ctx context.Context, functionID, data, idempotencyKey, deploymentID string) (*clientv1.InvokeFunctionResponse, error) {
	req := &clientv1.InvokeFunctionRequest{
		FunctionId:     functionID,
		Data:           data,
		IdempotencyKey: idempotencyKey,
	}
	if deploymentID != "" {
		req.DeploymentId = &deploymentID
	}
	return f.InvokeFunction(ctx, req)
}
