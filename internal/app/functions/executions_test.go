package functions

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestUC(executor *mockExecutor, repo *mockRepo, queue *mockQueue) *Functions {
	return NewFunctions(&config.AppConfig{}, executor, repo, queue)
}

func timeoutPtr(t int) *int { return &t }

func TestCreateExecution_SyncWritesResult(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	executor := newMockExecutor(&domainfunctions.ExecutionResult{
		StatusCode: 0,
		Stdout:     "hello",
		Response:   `{"ok":true}`,
		DurationMS: 42,
	}, nil)
	uc := newTestUC(executor, repo, newMockQueue())

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
		Data:       `{"a":1}`,
	})
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, rec.Status)
	require.Equal(t, "hello", rec.Stdout)
	require.Equal(t, `{"ok":true}`, rec.Response)
	require.Equal(t, int64(42), rec.DurationMS)
	require.Equal(t, "dep_ready", rec.DeploymentID)
	require.Len(t, executor.calls, 1)
	require.Equal(t, `{"a":1}`, executor.calls[0].Data, "data 透传给 executor")
}

func TestCreateExecution_SyncTruncatesOutput(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	big := string(make([]byte, maxOutputBytes+1024))
	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Stdout: big}, nil)
	uc := newTestUC(executor, repo, newMockQueue())

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, rec.Status)
	require.Len(t, rec.Stdout, maxOutputBytes)
	require.True(t, rec.StdoutTruncated)
	require.False(t, rec.ResponseTruncated)
}

func TestCreateExecution_SyncTimeoutMarksFailed(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	executor := newMockExecutor(nil, context.DeadlineExceeded)
	uc := newTestUC(executor, repo, newMockQueue())

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, repo.executions, 1)
	for _, e := range repo.executions {
		require.Equal(t, domainfunctions.ExecutionStatusFailed, e.Status)
		require.Equal(t, "execution timed out", e.Error)
	}
}

func TestCreateExecution_SyncExitCodeNonZeroFails(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 1, Stderr: "boom"}, nil)
	uc := newTestUC(executor, repo, newMockQueue())

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusFailed, rec.Status)
	require.Equal(t, "boom", rec.Error)
}

func TestCreateExecution_AsyncEnqueues(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	q := newMockQueue()
	uc := newTestUC(newMockExecutor(nil, nil), repo, q)

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
		Data:       `{"x":1}`,
		Async:      true,
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusQueued, rec.Status)
	require.Len(t, q.enqueued, 1)
	var msg queueMessage
	require.NoError(t, json.Unmarshal(q.enqueued[0], &msg))
	require.Equal(t, rec.ID, msg.ExecutionID)
	require.Equal(t, "fn_1", msg.FunctionID)
	require.Equal(t, "p1", msg.ProjectID)
	require.Equal(t, `{"x":1}`, msg.Data, "data 随队列 payload 传递")
	require.Equal(t, 0, msg.Attempt, "新入队 payload 无 attempt 字段（首次重试从 1 开始）")
}

func TestCreateExecution_EnqueueFailureMarksFailed(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	q := newMockQueue()
	q.err = errors.New("redis down")
	uc := newTestUC(newMockExecutor(nil, nil), repo, q)

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
		Async:      true,
	})
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Len(t, repo.executions, 1)
	var rec *domainfunctions.ExecutionRecord
	for _, e := range repo.executions {
		rec = e
	}
	require.Equal(t, domainfunctions.ExecutionStatusFailed, rec.Status)
	require.Equal(t, "enqueue failed", rec.Error)
}

func TestCreateExecution_NoReadyDeployment(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	// 移除 ready 部署。
	require.NoError(t, repo.DeleteDeployment(context.Background(), "p1", fn.ID, "dep_ready"))
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "no ready deployment")
}

func TestCreateExecution_ExplicitDeploymentNotReady(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	require.NoError(t, repo.CreateDeployment(context.Background(), &domainfunctions.Deployment{
		ID: "dep_pending", FunctionID: "fn_1", ProjectID: "p1", Status: domainfunctions.DeploymentStatusPending,
	}))
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID: "p1", FunctionID: "fn_1", DeploymentID: "dep_pending",
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "not ready")
}

func TestCreateExecution_DataTooLarge(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	big := make([]byte, maxExecutionDataBytes+1)
	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID: "p1", FunctionID: "fn_1", Data: string(big),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestCreateExecution_InvalidJSON(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID: "p1", FunctionID: "fn_1", Data: "{not json",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// G6-8/R07-P3-7：data 必须是 JSON object——数组/标量/字面量 null 一律拒绝。
func TestCreateExecution_DataMustBeJSONObject(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	for _, data := range []string{
		`[]`,    // 数组
		`"str"`, // 字符串
		`123`,   // 数字
		`true`,  // 布尔
		`null`,  // 字面量 null（解码成功但非 object）
		`[1,2]`, // 非空数组
		`"{}"`,  // 字符串化的 object 也不接受
	} {
		_, err := uc.CreateExecution(ctx, CreateExecutionCommand{
			ProjectID: "p1", FunctionID: "fn_1", Data: data,
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "data %q 必须被拒绝", data)
		require.ErrorContains(t, err, "JSON object")
	}

	// 空串（未提供）与合法 object 放行进入后续校验/执行。
	_, err := uc.CreateExecution(ctx, CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1", Data: ""})
	require.NoError(t, err)
	_, err = uc.CreateExecution(ctx, CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1", Data: `{"a":1}`})
	require.NoError(t, err)
	_, err = uc.CreateExecution(ctx, CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1", Data: `{}`})
	require.NoError(t, err)
}

func TestCreateExecution_SyncTimeoutOver30Rejected(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 60)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "use async")

	// 异步不受 30s 限制。
	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1", Async: true})
	require.NoError(t, err)
	require.NotNil(t, rec)
}

func TestCreateExecution_DisabledFunction(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", false, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestCreateExecution_NotFound(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "nope"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestCreateExecution_ResourceExhausted(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	sem, releases := newFullSemaphoreExec(maxConcurrentRuns)
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	uc.WithSemaphores(nil, sem)

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Len(t, repo.executions, 1)
	for _, e := range repo.executions {
		require.Equal(t, domainfunctions.ExecutionStatusFailed, e.Status)
		require.Equal(t, "too many concurrent executions", e.Error)
	}
}

func TestCreateFunction_Validation(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	_, err := uc.CreateFunction(ctx, CreateFunctionCommand{ID: "fn_x", ProjectID: "p1", Name: "f", Runtime: "bogus", TimeoutSeconds: timeoutPtr(15)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "unsupported runtime")

	_, err = uc.CreateFunction(ctx, CreateFunctionCommand{ID: "fn_x", ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(0)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = uc.CreateFunction(ctx, CreateFunctionCommand{ID: "fn_x", ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(301)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = uc.CreateFunction(ctx, CreateFunctionCommand{ID: "fn_x", ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(15), Spec: "bogus"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "unsupported spec")

	_, err = uc.CreateFunction(ctx, CreateFunctionCommand{ID: "fn_x", ProjectID: "p1", Name: "", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(15)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = uc.CreateFunction(ctx, CreateFunctionCommand{ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(15)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "id is required")

	fn, err := uc.CreateFunction(ctx, CreateFunctionCommand{
		ID: "fn_new", ProjectID: "p1", Name: "f", Runtime: "python-3.11", TimeoutSeconds: timeoutPtr(15),
	})
	require.NoError(t, err)
	require.Equal(t, "shared-1x", fn.Spec, "spec 缺省 shared-1x")
	require.Equal(t, "main.main", fn.Entrypoint, "python 缺省 entrypoint")
}

func TestCreateFunction_TimeoutDefault(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	fn, err := uc.CreateFunction(ctx, CreateFunctionCommand{
		ID: "fn_default", ProjectID: "p1", Name: "f", Runtime: "node-18.0",
	})
	require.NoError(t, err)
	require.Equal(t, defaultTimeoutSeconds, fn.TimeoutSeconds, "未显式指定 timeout_seconds 时应用服务端默认值")
	require.True(t, fn.Enabled, "未显式指定 enabled 时默认启用")
}

func TestCreateFunction_EnabledFalsePersists(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	disabled := false
	fn, err := uc.CreateFunction(ctx, CreateFunctionCommand{
		ID: "fn_disabled", ProjectID: "p1", Name: "f", Runtime: "node-18.0", Enabled: &disabled,
	})
	require.NoError(t, err)
	require.False(t, fn.Enabled, "显式 enabled=false 必须保留")
}

func TestProcessExecution_RebuildsWhenDeploymentNotReady(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	require.NoError(t, repo.DeleteDeployment(context.Background(), "p1", fn.ID, "dep_ready"))
	require.NoError(t, repo.CreateDeployment(context.Background(), &domainfunctions.Deployment{
		ID: "dep_pending", FunctionID: "fn_1", ProjectID: "p1", Status: domainfunctions.DeploymentStatusPending,
	}))

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Stdout: "ok"}, nil)
	uc := newTestUC(executor, repo, newMockQueue())

	rec := &domainfunctions.ExecutionRecord{
		ID: "e1", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_pending",
		Status: domainfunctions.ExecutionStatusQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.CreateExecution(context.Background(), rec))

	err := uc.ProcessExecutionPayload(context.Background(), []byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1","data":"{}"}`))
	require.NoError(t, err)
	require.Equal(t, 1, executor.builds, "deployment 非 ready 时补构建")
	dep, _ := repo.GetDeployment(context.Background(), "p1", "fn_1", "dep_pending")
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	got, _ := repo.GetExecution(context.Background(), "p1", "fn_1", "e1")
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, got.Status)
}

func TestProcessExecution_MissingExecutionSilentlyIgnored(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	err := uc.ProcessExecutionPayload(context.Background(), []byte(
		`{"execution_id":"ghost","function_id":"fn_1","project_id":"p1"}`))
	require.NoError(t, err)
}

func TestProcessExecution_InvalidPayload(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	err := uc.ProcessExecutionPayload(context.Background(), []byte("not json"))
	require.Error(t, err)
}

// 2026-08 评审 P0-1：重复投递收敛——终态/在途记录一律跳过，不重复执行。
func TestProcessExecution_DuplicateDeliverySkipsTerminal(t *testing.T) {
	for _, st := range []string{
		domainfunctions.ExecutionStatusCompleted,
		domainfunctions.ExecutionStatusFailed,
		domainfunctions.ExecutionStatusRunning,
		domainfunctions.ExecutionStatusBuilding,
	} {
		repo := newMockRepo()
		seedReadyFunction(repo, "p1", "fn_1", true, 15)
		executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
		uc := newTestUC(executor, repo, newMockQueue())
		rec := &domainfunctions.ExecutionRecord{
			ID: "e1", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_ready",
			Status: st, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		require.NoError(t, repo.CreateExecution(context.Background(), rec))

		err := uc.ProcessExecutionPayload(context.Background(), []byte(
			`{"execution_id":"e1","function_id":"fn_1","project_id":"p1"}`))
		require.NoError(t, err, "status %s 的重复投递必须静默跳过", st)
		require.Empty(t, executor.calls, "status %s 不得触发执行", st)
		got, _ := repo.GetExecution(context.Background(), "p1", "fn_1", "e1")
		require.Equal(t, st, got.Status, "status %s 不得被改写", st)
	}
}

// 补构建遇信号量满（ResourceExhausted）：归还 queued 供 requeue 重试，且不得
// 删除既有 deployment 行（2026-08 评审 P0-2）。信号量释放后同一执行重试成功。
func TestProcessExecution_SemaphoreFullReleasesAndKeepsDeployment(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	require.NoError(t, repo.DeleteDeployment(context.Background(), "p1", fn.ID, "dep_ready"))
	require.NoError(t, repo.CreateDeployment(context.Background(), &domainfunctions.Deployment{
		ID: "dep_pending", FunctionID: "fn_1", ProjectID: "p1", Status: domainfunctions.DeploymentStatusPending,
	}))

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Stdout: "ok"}, nil)
	uc := newTestUC(executor, repo, newMockQueue())
	sem, releases := newFullSemaphoreExec(maxConcurrentBuilds)
	uc.WithSemaphores(sem, nil)
	rec := &domainfunctions.ExecutionRecord{
		ID: "e1", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_pending",
		Status: domainfunctions.ExecutionStatusQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.CreateExecution(context.Background(), rec))

	// 占满构建信号量：worker 补构建路径拿到 ResourceExhausted。
	err := uc.ProcessExecutionPayload(context.Background(), []byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1"}`))
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "可重试失败必须上抛，由 worker requeue 兜底")
	for _, r := range releases {
		r()
	}

	got, _ := repo.GetExecution(context.Background(), "p1", "fn_1", "e1")
	require.Equal(t, domainfunctions.ExecutionStatusQueued, got.Status, "归还 queued 供重试再次领取")

	dep, _ := repo.GetDeployment(context.Background(), "p1", "fn_1", "dep_pending")
	require.NotNil(t, dep, "既有 deployment 行不得被信号量满删除（P0-2）")
	require.Equal(t, domainfunctions.DeploymentStatusPending, dep.Status, "deployment 状态不得被改动")
	require.Empty(t, executor.calls, "构建失败不得进入执行")

	// 信号量释放后重试成功。
	require.NoError(t, uc.ProcessExecutionPayload(context.Background(), []byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1"}`)))
	got, _ = repo.GetExecution(context.Background(), "p1", "fn_1", "e1")
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, got.Status)
	dep, _ = repo.GetDeployment(context.Background(), "p1", "fn_1", "dep_pending")
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
}

// 第三次重复投递（completed 之后）：不再执行、不再计费用量。
func TestProcessExecution_CompletedNotReexecuted(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, DurationMS: 42}, nil)
	uc := newTestUC(executor, repo, newMockQueue())
	rec := &domainfunctions.ExecutionRecord{
		ID: "e1", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_ready",
		Status: domainfunctions.ExecutionStatusQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.CreateExecution(context.Background(), rec))

	require.NoError(t, uc.ProcessExecutionPayload(context.Background(), []byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1"}`)))
	require.Len(t, executor.calls, 1)

	// 重复投递 ×2。
	require.NoError(t, uc.ProcessExecutionPayload(context.Background(), []byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1"}`)))
	require.NoError(t, uc.ProcessExecutionPayload(context.Background(), []byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1"}`)))
	require.Len(t, executor.calls, 1, "completed 记录不得重复执行")
}

// 2026-08 评审 P1-7：MarkExecutionFailed 不覆盖终态。
func TestMarkExecutionFailed_DoesNotOverwriteTerminal(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	completed := &domainfunctions.ExecutionRecord{
		ID: "e-done", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_ready",
		Status: domainfunctions.ExecutionStatusCompleted, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.CreateExecution(context.Background(), completed))
	require.NoError(t, uc.MarkExecutionFailed(context.Background(), "p1", "fn_1", "e-done", "worker retries exhausted"))
	got, _ := repo.GetExecution(context.Background(), "p1", "fn_1", "e-done")
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, got.Status, "completed 不被改写")

	queued := &domainfunctions.ExecutionRecord{
		ID: "e-q", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_ready",
		Status: domainfunctions.ExecutionStatusQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.CreateExecution(context.Background(), queued))
	require.NoError(t, uc.MarkExecutionFailed(context.Background(), "p1", "fn_1", "e-q", "worker retries exhausted"))
	got, _ = repo.GetExecution(context.Background(), "p1", "fn_1", "e-q")
	require.Equal(t, domainfunctions.ExecutionStatusFailed, got.Status, "queued 正常标 failed")
	require.Equal(t, "worker retries exhausted", got.Error)
}

type recUsage struct{ n int64 }

func (r *recUsage) Incr(_ context.Context, _ string, metric string, delta int64) error {
	if metric == domainbilling.MetricFunctionDurationMS {
		r.n += delta
	}
	return nil
}
func (r *recUsage) IncrAt(ctx context.Context, projectID, metric string, _ time.Time, delta int64) error {
	return r.Incr(ctx, projectID, metric, delta)
}
func (r *recUsage) Set(context.Context, string, string, time.Time, int64) error { return nil }
func (r *recUsage) Get(context.Context, string, string, time.Time) (int64, error) {
	return 0, nil
}
func (r *recUsage) ListHour(context.Context, time.Time) ([]domainbilling.Bucket, error) {
	return nil, nil
}

func TestCreateExecution_MetersFunctionDuration(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, DurationMS: 42}, nil)
	meter := &recUsage{}
	uc := NewFunctionsWithUsage(&config.AppConfig{}, executor, repo, newMockQueue(), meter, nil, Semaphores{}, nil, nil)

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)
	require.Equal(t, int64(42), rec.DurationMS)
	require.Equal(t, int64(42), meter.n)
}

type stubProjectRepo struct {
	list []domainprojects.Project
}

func (s *stubProjectRepo) CreateProject(context.Context, *domainprojects.Project) error {
	return nil
}
func (s *stubProjectRepo) GetProject(context.Context, string) (*domainprojects.Project, error) {
	return nil, nil
}
func (s *stubProjectRepo) GetProjectByName(context.Context, string) (*domainprojects.Project, error) {
	return nil, nil
}
func (s *stubProjectRepo) ListProjects(context.Context) ([]domainprojects.Project, error) {
	return s.list, nil
}
func (s *stubProjectRepo) UpdateProject(context.Context, *domainprojects.Project) error {
	return nil
}
func (s *stubProjectRepo) DeleteProject(context.Context, string) error                 { return nil }
func (s *stubProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }

func TestRecoverOrphanExecutions_EnumeratesActiveProjectsWithBudget(t *testing.T) {
	repo := newMockRepo()
	repo.recoverEach = 300
	projects := &stubProjectRepo{list: []domainprojects.Project{
		{ID: "p1", Status: "active"},
		{ID: "p-off", Status: "suspended"},
		{ID: "p2", Status: "active"},
	}}
	uc := NewFunctionsWithUsage(&config.AppConfig{}, newMockExecutor(nil, nil), repo, newMockQueue(), nil, projects, Semaphores{}, nil, nil)

	n, err := uc.RecoverOrphanExecutions(context.Background(), time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(500), n, "全局预算 500：p1 扣 300，p2 扣 remaining 200")
	require.Equal(t, []string{"p1", "p2"}, repo.recoverCalls)
	require.Equal(t, []int{500, 200}, repo.recoverLimits)
}

func newFullSemaphoreExec(max int) (*semaphore.InMemorySemaphore, []func()) {
	sem := semaphore.NewInMemory(max)
	var releases []func()
	for i := 0; i < max; i++ {
		ok, rel, _ := sem.TryAcquire(context.Background())
		if ok {
			releases = append(releases, rel)
		}
	}
	return sem, releases
}
