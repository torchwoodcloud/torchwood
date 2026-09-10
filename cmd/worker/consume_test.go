package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// retryRepo 是 FunctionRepo 的测试桩：GetExecution 返回固定记录，GetFunction
// 注入瞬时错误（驱动重试路径），并记录 UpdateExecution 调用。
type retryRepo struct {
	mu       sync.Mutex
	rec      *domainfunctions.ExecutionRecord
	getFnErr error
	updates  []*domainfunctions.ExecutionRecord
}

func (r *retryRepo) CreateFunction(context.Context, *domainfunctions.Function) error { return nil }
func (r *retryRepo) GetFunction(context.Context, string, string) (*domainfunctions.Function, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return nil, r.getFnErr
}
func (r *retryRepo) ListFunctions(context.Context, string) ([]domainfunctions.Function, error) {
	return nil, nil
}
func (r *retryRepo) UpdateFunction(context.Context, *domainfunctions.Function) error { return nil }
func (r *retryRepo) DeleteFunction(context.Context, string, string) error            { return nil }
func (r *retryRepo) CreateDeployment(context.Context, *domainfunctions.Deployment) error {
	return nil
}
func (r *retryRepo) GetDeployment(context.Context, string, string, string) (*domainfunctions.Deployment, error) {
	return nil, nil
}
func (r *retryRepo) ListDeployments(context.Context, string, string) ([]domainfunctions.Deployment, error) {
	return nil, nil
}
func (r *retryRepo) UpdateDeployment(context.Context, *domainfunctions.Deployment) error { return nil }

func (r *retryRepo) ActivateDeployment(context.Context, *domainfunctions.Deployment) error {
	return nil
}
func (r *retryRepo) DeleteDeployment(context.Context, string, string, string) error { return nil }
func (r *retryRepo) SetVariables(context.Context, string, string, map[string]string) error {
	return nil
}
func (r *retryRepo) GetVariables(context.Context, string, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *retryRepo) CreateExecution(context.Context, *domainfunctions.ExecutionRecord) error {
	return nil
}
func (r *retryRepo) GetExecution(context.Context, string, string, string) (*domainfunctions.ExecutionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec == nil {
		return nil, nil
	}
	cp := *r.rec
	return &cp, nil
}
func (r *retryRepo) ListExecutions(context.Context, string, string, int) ([]domainfunctions.ExecutionRecord, error) {
	return nil, nil
}
func (r *retryRepo) UpdateExecution(_ context.Context, e *domainfunctions.ExecutionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *e
	r.updates = append(r.updates, &cp)
	r.rec = &cp
	return nil
}
func (r *retryRepo) TransitionExecutionStatus(_ context.Context, _, _, _ string, from, to string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec == nil || r.rec.Status != from {
		return false, nil
	}
	r.rec.Status = to
	return true, nil
}
func (r *retryRepo) FailExecutionIfActive(_ context.Context, _, _, _, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec == nil {
		return nil
	}
	switch r.rec.Status {
	case domainfunctions.ExecutionStatusQueued,
		domainfunctions.ExecutionStatusBuilding,
		domainfunctions.ExecutionStatusRunning:
		r.rec.Status = domainfunctions.ExecutionStatusFailed
		r.rec.Error = reason
	}
	return nil
}
func (r *retryRepo) RecoverOrphanExecutionsInProject(context.Context, string, time.Time, int) (int64, error) {
	return 0, nil
}
func (r *retryRepo) PruneOldExecutionsInProject(context.Context, string, string, int) error {
	return nil
}
func (r *retryRepo) PruneTriggerExecutionsInProject(context.Context, string, string, time.Time) error {
	return nil
}
func (r *retryRepo) GetExecutionByIdempotencyKey(context.Context, string, string, string, string) (*domainfunctions.ExecutionRecord, error) {
	return nil, nil
}
func (r *retryRepo) CountClientInvocations(context.Context, string, string, string, time.Time) (int, error) {
	return 0, nil
}

// retryExecutor 是 functions.Executor 的零值桩（重试路径在 GetFunction 即
// 失败，不会触达执行）。
type retryExecutor struct{}

func (retryExecutor) Execute(context.Context, domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	return nil, nil
}
func (retryExecutor) Build(context.Context, string, string, string) error { return nil }
func (retryExecutor) RemoveImage(context.Context, string, string) error   { return nil }

// channelQueue 是 shared.Queue 的测试桩：Enqueue 记录 payload 并送入有缓冲
// channel，Dequeue 取出；空时按 timeout 返回 nil（与真实 BRPOP 语义对齐）。
type channelQueue struct {
	mu       sync.Mutex
	ch       chan []byte
	enqueued [][]byte
}

func newChannelQueue() *channelQueue {
	return &channelQueue{ch: make(chan []byte, 64)}
}

func (q *channelQueue) Trim(context.Context, string, int64) error { return nil }

func (q *channelQueue) Enqueue(_ context.Context, _ string, payload []byte) error {
	q.mu.Lock()
	q.enqueued = append(q.enqueued, payload)
	q.mu.Unlock()
	q.ch <- payload
	return nil
}

func (q *channelQueue) Dequeue(ctx context.Context, _ string, timeout time.Duration) ([]byte, string, error) {
	select {
	case p := <-q.ch:
		return p, "ack-" + string(p), nil
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case <-time.After(timeout):
		return nil, "", nil
	}
}

func (q *channelQueue) Ack(_ context.Context, _ string, _ string) error { return nil }

// TestConsume_RetryAttemptsInPayloadAndExhausts 驱动 consume 循环验证（B2）：
// 瞬时失败任务重抛回队时 payload 携带递增 attempt（计数随队列消息持久，
// 重启/多副本不丢）；达到 maxProcessAttempts 上限（第 4 次失败，=3 语义）
// 后不再 Enqueue，改由 MarkExecutionFailed 兜底标 failed。
func TestConsume_RetryAttemptsInPayloadAndExhausts(t *testing.T) {
	repo := &retryRepo{
		rec: &domainfunctions.ExecutionRecord{
			ID: "e1", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_1",
			Status: domainfunctions.ExecutionStatusQueued,
		},
		getFnErr: errors.New("db unavailable"), // 瞬时失败
	}
	q := newChannelQueue()
	fn := appfunctions.NewFunctions(&config.AppConfig{}, retryExecutor{}, repo, q)
	w := NewWorker(fn, q, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.consume(ctx)
	}()

	// 初始 payload（旧格式，无 attempt 字段）。
	q.ch <- []byte(`{"execution_id":"e1","function_id":"fn_1","project_id":"p1","data":"{\"x\":1}"}`)

	// 等待兜底 MarkExecutionFailed（第 4 次失败超限）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		repo.mu.Lock()
		failed := repo.rec != nil && repo.rec.Status == domainfunctions.ExecutionStatusFailed
		repo.mu.Unlock()
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待 MarkExecutionFailed 超时")
		}
		time.Sleep(5 * time.Millisecond)
	}

	repo.mu.Lock()
	require.Equal(t, domainfunctions.ExecutionStatusFailed, repo.rec.Status)
	require.Contains(t, repo.rec.Error, "worker retries exhausted")
	repo.mu.Unlock()

	q.mu.Lock()
	defer q.mu.Unlock()
	require.Len(t, q.enqueued, maxProcessAttempts, "达到上限后不应再 Enqueue")
	attempts := make([]int, 0, len(q.enqueued))
	for _, p := range q.enqueued {
		var m retryMessage
		require.NoError(t, json.Unmarshal(p, &m))
		require.Equal(t, "e1", m.ExecutionID)
		require.Equal(t, "fn_1", m.FunctionID)
		require.Equal(t, "p1", m.ProjectID)
		require.Equal(t, `{"x":1}`, m.Data, "data 在重试往返中不丢失")
		attempts = append(attempts, m.Attempt)
	}
	require.Equal(t, []int{1, 2, 3}, attempts, "重试计数应随 payload 递增")

	cancel()
	<-done
}

// TestConsume_OldFormatPayloadRetriesOnce 旧格式 payload（无 attempt 字段）
// 经 consume 重试后 attempt=1（与 requeue 单测一致，跨重启兼容）。
func TestConsume_OldFormatPayloadRetriesOnce(t *testing.T) {
	repo := &retryRepo{
		rec: &domainfunctions.ExecutionRecord{
			ID: "e1", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_1",
			Status: domainfunctions.ExecutionStatusQueued,
		},
		getFnErr: errors.New("db unavailable"),
	}
	q := newChannelQueue()
	fn := appfunctions.NewFunctions(&config.AppConfig{}, retryExecutor{}, repo, q)
	w := NewWorker(fn, q, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.consume(ctx)
	}()

	q.ch <- []byte(`{"execution_id":"e1","function_id":"fn_1","project_id":"p1","data":"{}"}`)

	// 等待首个重试入队。
	deadline := time.Now().Add(5 * time.Second)
	for {
		q.mu.Lock()
		n := len(q.enqueued)
		q.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待首次重试 Enqueue 超时")
		}
		time.Sleep(5 * time.Millisecond)
	}

	q.mu.Lock()
	var m retryMessage
	require.NoError(t, json.Unmarshal(q.enqueued[0], &m))
	q.mu.Unlock()
	require.Equal(t, 1, m.Attempt, "旧格式 payload 首次重试 attempt=1")
	require.Equal(t, `{}`, m.Data)

	cancel()
	<-done
}
