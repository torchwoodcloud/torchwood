package clientgrpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// ---- handler 层薄桩（use-case 全量语义在 internal/app/functions 测试覆盖）----
// v4：函数 = endpoint 常驻服务；客户端调用走 RPC 轴 Invoker（invoke method）。

type stubFnRepo struct {
	domainfunctions.FunctionRepo
	fn      *domainfunctions.Function
	created []*domainfunctions.InvocationRecord
	updated []*domainfunctions.InvocationRecord
}

func (r *stubFnRepo) GetFunction(_ context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	if r.fn == nil || r.fn.ProjectID != projectID || r.fn.ID != functionID {
		return nil, nil
	}
	return r.fn, nil
}

func (r *stubFnRepo) CreateInvocation(_ context.Context, e *domainfunctions.InvocationRecord) error {
	r.created = append(r.created, e)
	return nil
}

func (r *stubFnRepo) UpdateInvocation(_ context.Context, e *domainfunctions.InvocationRecord) error {
	r.updated = append(r.updated, e)
	return nil
}

func (r *stubFnRepo) CountClientInvocations(context.Context, string, string, string, time.Time) (int, error) {
	return 0, nil
}

type stubFnInvoker struct{}

func (stubFnInvoker) Invoke(_ context.Context, _ domainfunctions.Invocation) (*domainfunctions.InvocationResult, error) {
	return &domainfunctions.InvocationResult{Response: `{"ok":true}`}, nil
}

type stubFnNotifier struct{}

func (stubFnNotifier) Notify(context.Context, domainfunctions.Notification) (domainfunctions.NotificationResult, error) {
	return domainfunctions.NotificationResult{}, nil
}

type stubFnQueue struct{}

func (stubFnQueue) Trim(context.Context, string, int64) error     { return nil }
func (stubFnQueue) Enqueue(context.Context, string, []byte) error { return nil }
func (stubFnQueue) Dequeue(context.Context, string, time.Duration) ([]byte, string, error) {
	return nil, "", nil
}
func (stubFnQueue) Ack(context.Context, string, string) error { return nil }

// TestClientGRPC_InvokeFunction（P2 客户端调用面 handler）：principal 的
// project/user 注入命令、调用记录携带 invoking_user_id 与 trigger_source，
// 响应映射 execution_id/status/response。
func TestClientGRPC_InvokeFunction(t *testing.T) {
	repo := &stubFnRepo{fn: &domainfunctions.Function{
		ID: "fn-1", ProjectID: "proj-1", Name: "fn",
		TargetURL: "https://fn.example.com",
		Enabled:   true, ClientCallable: true, ClientPerUserLimit: 10,
		ClientLimitWindow: domainfunctions.ClientLimitWindowDay,
	}}
	uc := appfunctions.NewFunctions(&config.AppConfig{}, stubFnInvoker{}, stubFnNotifier{}, repo, stubFnQueue{}, nil, nil, nil, nil, nil)
	svc := NewFunctionsService(uc)

	resp, err := svc.InvokeFunction(clientCtx(), &clientv1.InvokeFunctionRequest{
		FunctionId:     "fn-1",
		Data:           `{"a":1}`,
		IdempotencyKey: "idem-1",
	})
	require.NoError(t, err)
	require.Equal(t, "completed", resp.Status)
	require.Equal(t, `{"ok":true}`, resp.Response)
	require.Len(t, repo.created, 1)
	require.Equal(t, "u1", repo.created[0].InvokingUserID, "调用用户应落调用记录")
	require.Equal(t, domainfunctions.TriggerSourceClient, repo.created[0].TriggerSource)

	// 非 client_callable → PermissionDenied 透传（独立用例实例：函数记录
	// 会进 use-case 30s 缓存，直接改 stub 行不影响已缓存条目）。
	deniedRepo := &stubFnRepo{fn: &domainfunctions.Function{
		ID: "fn-1", ProjectID: "proj-1", Name: "fn",
		TargetURL: "https://fn.example.com",
		Enabled:   true, ClientCallable: false,
	}}
	deniedSvc := NewFunctionsService(appfunctions.NewFunctions(&config.AppConfig{}, stubFnInvoker{}, stubFnNotifier{}, deniedRepo, stubFnQueue{}, nil, nil, nil, nil, nil))
	_, err = deniedSvc.InvokeFunction(clientCtx(), &clientv1.InvokeFunctionRequest{FunctionId: "fn-1"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// 缺 project 上下文 → Unauthenticated。
	_, err = svc.InvokeFunction(context.Background(), &clientv1.InvokeFunctionRequest{FunctionId: "fn-1"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
