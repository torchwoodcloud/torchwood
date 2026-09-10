package functions

import (
	"context"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// ——v3 实例内多路复用（docs/design/functions-v3.md §1.5 策略链路 + 降级保护）
// 的 app 层单测：concurrency 从函数记录到执行请求的透传与 template_version
// 降级判定——

// v2UC 构造 executor=dispatcher 的用例（v2 常驻执行模型）。
func v2UC(executor *mockExecutor, repo *mockRepo, queue *mockQueue) *Functions {
	return NewFunctions(&config.AppConfig{Functions: &config.Functions{Executor: "dispatcher"}}, executor, repo, queue)
}

// downgradedCounter 取指定标签的降级计数当前值（零值返回 0）。
func downgradedCounter(projectID, functionID string) float64 {
	return promtestutil.ToFloat64(concurrencyDowngradedTotal.WithLabelValues(projectID, functionID))
}

// TestBuildExecution_ConcurrencyAndExecutionIDPassthrough 透传断言（v3 §1.5）：
// v3 模板下 fn.Concurrency 原样进 Execution.Concurrency，预占 INSERT 生成的
// 执行 ID 进 Execution.ExecutionID（经分发 header x-tw-execution-id 透传给
// runner 的 ctx.executionId）。
func TestBuildExecution_ConcurrencyAndExecutionIDPassthrough(t *testing.T) {
	uc := v2UC(newMockExecutor(nil, nil), newMockRepo(), newMockQueue())
	fn := &domainfunctions.Function{ID: "fn_c", ProjectID: "p1", Concurrency: 8}
	rec := &domainfunctions.ExecutionRecord{ID: "exe_1", DeploymentID: "dep_ready"}

	exec := uc.buildExecution(fn, rec, domainfunctions.RunnerTemplateVersion, nil, "{}", "", "", nil, nil, false)
	require.Equal(t, 8, exec.Concurrency, "v3 模板：并发原样透传")
	require.Equal(t, "exe_1", exec.ExecutionID, "执行 ID 透传给 runner ctx.executionId")
	require.Nil(t, exec.TriggerEnvelope, "非触发器调用不携带封套")
	require.Nil(t, exec.RawBody, "非触发器调用无 RawBody")
}

// TestCreateExecution_ConcurrencyDowngradedOnOldTemplate 降级保护（v3 §1.5
// 「fail-safe 不 fail-closed」）：template_version=2 + concurrency=8 → 按并发
// 1 执行 + 降级计数 +1；存量函数不因新列拒绝执行。
func TestCreateExecution_ConcurrencyDowngradedOnOldTemplate(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_dg", true, 15)
	fn.Concurrency = 8
	_ = repo.CreateFunction(context.Background(), fn)
	// 存量 deployment：v2 模板（TemplateVersion < RunnerTemplateVersion）。
	repo.deployments["dep_ready"].TemplateVersion = 2

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Response: `{}`}, nil)
	uc := v2UC(executor, repo, newMockQueue())

	before := downgradedCounter("p1", "fn_dg")
	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_dg", Data: `{}`})
	require.NoError(t, err)
	require.Len(t, executor.calls, 1)
	require.Equal(t, 1, executor.calls[0].Concurrency, "v2 模板降级按并发 1 执行")
	require.Equal(t, rec.ID, executor.calls[0].ExecutionID, "降级不影响执行 ID 透传")
	require.InDelta(t, before+1, downgradedCounter("p1", "fn_dg"), 0.001, "降级计数 +1")
}

// TestCreateExecution_ConcurrencyKeptOnV3Template v3 模板：concurrency=8
// 原样透传，降级计数不动。
func TestCreateExecution_ConcurrencyKeptOnV3Template(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_v3", true, 15)
	fn.Concurrency = 8
	_ = repo.CreateFunction(context.Background(), fn)
	repo.deployments["dep_ready"].TemplateVersion = domainfunctions.RunnerTemplateVersion

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Response: `{}`}, nil)
	uc := v2UC(executor, repo, newMockQueue())

	before := downgradedCounter("p1", "fn_v3")
	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_v3", Data: `{}`})
	require.NoError(t, err)
	require.Len(t, executor.calls, 1)
	require.Equal(t, 8, executor.calls[0].Concurrency, "v3 模板：并发=8 原样透传")
	require.InDelta(t, before, downgradedCounter("p1", "fn_v3"), 0.001, "无降级计数")
}

// TestProcessExecution_ConcurrencyDowngradedOnOldTemplate 异步路径同一降级
// 语义：ProcessExecution 自行加载 deployment，TemplateVersion 从该行判定。
func TestProcessExecution_ConcurrencyDowngradedOnOldTemplate(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_async", true, 15)
	fn.Concurrency = 8
	_ = repo.CreateFunction(context.Background(), fn)
	repo.deployments["dep_ready"].TemplateVersion = 2

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Response: `{}`}, nil)
	uc := v2UC(executor, repo, newMockQueue())

	now := time.Now()
	rec := &domainfunctions.ExecutionRecord{
		ID: "exe_async", FunctionID: "fn_async", ProjectID: "p1", DeploymentID: "dep_ready",
		Status: domainfunctions.ExecutionStatusQueued, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, repo.CreateExecution(context.Background(), rec))

	before := downgradedCounter("p1", "fn_async")
	require.NoError(t, uc.ProcessExecution(context.Background(), queueMessage{
		ExecutionID: rec.ID, FunctionID: "fn_async", ProjectID: "p1",
	}))
	require.Len(t, executor.calls, 1)
	require.Equal(t, 1, executor.calls[0].Concurrency, "异步路径同样降级按 1")
	require.InDelta(t, before+1, downgradedCounter("p1", "fn_async"), 0.001, "异步路径降级计数 +1")
}

// TestCreateExecution_V1ExecutorNoDowngradeMetric v1 docker executor 无池概念
// （v3 §1.6），Concurrency 本就被忽略：降级分支不记账（避免噪音指标）。
func TestCreateExecution_V1ExecutorNoDowngradeMetric(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_v1", true, 15)
	fn.Concurrency = 8
	_ = repo.CreateFunction(context.Background(), fn)
	repo.deployments["dep_ready"].TemplateVersion = 2

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Response: `{}`}, nil)
	uc := newTestUC(executor, repo, newMockQueue()) // executor 未配置 = v1 docker

	before := downgradedCounter("p1", "fn_v1")
	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_v1", Data: `{}`})
	require.NoError(t, err)
	require.InDelta(t, before, downgradedCounter("p1", "fn_v1"), 0.001, "v1 路径不记降级计数")
}
