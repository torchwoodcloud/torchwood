package functions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---- v3 切片 C：触发器封套透传与 fetch 风格执行结果（functions-v3.md §2.2/§2.3/D10）----

func envelopeUC(executor *mockExecutor, repo *mockRepo, queue *mockQueue) *Functions {
	return NewFunctions(&config.AppConfig{Functions: &config.Functions{Executor: "dispatcher"}}, executor, repo, queue)
}

func httpEnvelope() *domainfunctions.TriggerEnvelope {
	return &domainfunctions.TriggerEnvelope{
		Method:   http.MethodPost,
		Path:     "/f/p1/tok1",
		RawQuery: "signature=abc",
		Headers:  map[string][]string{"x-wx-signature": {"sig1"}, "content-type": {"application/xml"}},
	}
}

// TestInvokeTrigger_EnvelopePassthrough HTTP 触发器恒填充透传（v3 §2.3/D10）：
// InvokeTrigger 的 TriggerEnvelope/RawBody 原样进 Execution（sync 路径）；
// TW_DATA 封套照旧进 Data（main 风格双轨 D9）。
func TestInvokeTrigger_EnvelopePassthrough(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_env", true, 15)
	_ = repo.CreateFunction(context.Background(), fn)
	repo.deployments["dep_ready"].TemplateVersion = domainfunctions.RunnerTemplateVersion

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Response: `{}`}, nil)
	uc := envelopeUC(executor, repo, newMockQueue())

	rawBody := []byte(`<xml>raw</xml>`)
	_, err := uc.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID:       "p1",
		FunctionID:      "fn_env",
		Data:            `{"method":"POST","body":"<xml>raw</xml>"}`,
		Source:          "http:trg-1",
		BodyLimitBytes:  64 << 10,
		TriggerEnvelope: httpEnvelope(),
		RawBody:         rawBody,
	})
	require.NoError(t, err)
	require.Len(t, executor.calls, 1)
	require.Equal(t, httpEnvelope(), executor.calls[0].TriggerEnvelope, "封套元数据原样透传")
	require.Equal(t, rawBody, executor.calls[0].RawBody, "原始 body 原样透传")
}

// TestInvokeTrigger_EnvelopeAsyncQueue async_ack 路径：封套元数据 + 原始 body
// 随队列 payload 透传，worker ProcessExecution 还原进 Execution。
func TestInvokeTrigger_EnvelopeAsyncQueue(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_async_env", true, 15)
	_ = repo.CreateFunction(context.Background(), fn)
	repo.deployments["dep_ready"].TemplateVersion = domainfunctions.RunnerTemplateVersion

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Response: `{}`}, nil)
	uc := envelopeUC(executor, repo, newMockQueue())

	rawBody := []byte(`binary-\x00-payload`)
	_, err := uc.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID:       "p1",
		FunctionID:      "fn_async_env",
		Data:            `{}`,
		Async:           true,
		Source:          "http:trg-2",
		TriggerEnvelope: httpEnvelope(),
		RawBody:         rawBody,
	})
	require.NoError(t, err)

	// 队列 payload 携带封套；worker 消费后进 Execution。
	var msg queueMessage
	require.Len(t, uc.queue.(*mockQueue).enqueued, 1)
	require.NoError(t, json.Unmarshal(uc.queue.(*mockQueue).enqueued[0], &msg))
	require.Equal(t, httpEnvelope(), msg.TriggerEnvelope)
	require.Equal(t, rawBody, msg.RawBody)

	executor.calls = nil
	require.NoError(t, uc.ProcessExecution(context.Background(), msg))
	require.Len(t, executor.calls, 1)
	require.Equal(t, httpEnvelope(), executor.calls[0].TriggerEnvelope)
	require.Equal(t, rawBody, executor.calls[0].RawBody)
}

// TestRunExecution_FetchStyleHTTPStatus v4 fetch 风格结果（v3 §2.2/D10）：
// StatusCode = 函数 HTTP status（非 2xx 也是合法结果 → completed）；body 双
// 通道与 headers 进执行记录的运行期字段（不落库由 repo 显式映射保证）。
func TestRunExecution_FetchStyleHTTPStatus(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_fetch", true, 15)
	_ = repo.CreateFunction(context.Background(), fn)
	repo.deployments["dep_ready"].TemplateVersion = domainfunctions.RunnerTemplateVersion

	executor := newMockExecutor(&domainfunctions.ExecutionResult{
		StatusCode:  404,
		Response:    "nope",
		ResponseB64: base64.StdEncoding.EncodeToString([]byte("nope")),
		Headers:     map[string]string{"content-type": "text/plain"},
	}, nil)
	uc := envelopeUC(executor, repo, newMockQueue())

	rec, err := uc.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID:       "p1",
		FunctionID:      "fn_fetch",
		Data:            `{}`,
		Source:          "http:trg-3",
		TriggerEnvelope: httpEnvelope(),
		RawBody:         []byte("x"),
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, rec.Status, "函数 HTTP 4xx 是一等结果，不映射执行失败")
	require.Equal(t, 404, rec.StatusCode, "rec.StatusCode 承载函数 HTTP status")
	require.Equal(t, "nope", rec.Response)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("nope")), rec.ResponseB64)
	require.Equal(t, map[string]string{"content-type": "text/plain"}, rec.HTTPHeaders)
}

// TestRunExecution_V1ExitCodeStillFails v1 退出码语义回归：非 v2 executor 下
// 非零 StatusCode 仍映射 failed（err==nil 形态）。
func TestRunExecution_V1ExitCodeStillFails(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_v1ec", true, 15)
	_ = repo.CreateFunction(context.Background(), fn)

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 1, Stderr: "boom"}, nil)
	uc := newTestUC(executor, repo, newMockQueue()) // executor 未配置 = v1 docker

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_v1ec", Data: `{}`})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusFailed, rec.Status)
	require.Equal(t, 1, rec.StatusCode)
}

// TestInvokeTrigger_FailureUnknown 函数执行失败封套（dispatcher Unknown）在
// 触发器路径映射 BadGateway 由 handler 完成——此处确认错误以 Unknown 透传
// 到 handler（v3 §2.2「runner ok=false 仍 502+错误语义」）。
func TestInvokeTrigger_FailureUnknown(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_fail", true, 15)
	_ = repo.CreateFunction(context.Background(), fn)
	repo.deployments["dep_ready"].TemplateVersion = domainfunctions.RunnerTemplateVersion

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 1}, status.Error(codes.Unknown, "function failed"))
	uc := envelopeUC(executor, repo, newMockQueue())

	_, err := uc.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID:       "p1",
		FunctionID:      "fn_fail",
		Data:            `{}`,
		Source:          "http:trg-4",
		TriggerEnvelope: httpEnvelope(),
		RawBody:         []byte("x"),
	})
	require.Error(t, err)
	require.Equal(t, codes.Unknown, status.Code(err))
}

// TestCreateExecution_RawBodyGuard RawBody 通道防御（v3 §2.3）：超上限
// InvalidArgument；413 判定仍在 handler 入口（app 层只防异常直调）。
func TestCreateExecution_RawBodyGuard(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_guard", true, 15)
	_ = repo.CreateFunction(context.Background(), fn)

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Response: `{}`}, nil)
	uc := envelopeUC(executor, repo, newMockQueue())

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID:       "p1",
		FunctionID:      "fn_guard",
		Data:            `{}`,
		TriggerEnvelope: httpEnvelope(),
		RawBody:         make([]byte, maxTriggerDataBytes+1),
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
