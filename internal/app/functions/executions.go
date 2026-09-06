package functions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	domainbilling "github.com/torchwooddev/torchwood/internal/domain/billing"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 执行限制（§5.3 / §9.6）。
const (
	maxExecutionDataBytes = 32 << 10 // data ≤ 32KB（execve 单变量硬限制余量）
	maxEnvBytes           = 32 << 10 // env vars 总量 ≤ 32KB
	maxOutputBytes        = 64 << 10 // stdout/stderr/response 截断上限
	pruneKeepRecent       = 100      // 保留策略：每函数最多保留最近 100 条
	recoverOrphanBatch    = 500      // worker 启动对账全局预算（K22）
	// workerRebuildTimeout 是 worker 补构建的最长耗时（防挂死的 daemon 卡住消费）。
	workerRebuildTimeout = 5 * time.Minute
)

// ErrInvalidQueuePayload 标识无法解析或缺失 ID 的队列消息（worker 不应重试）。
var ErrInvalidQueuePayload = errors.New("invalid queue payload")

// 执行信号量：同步执行与 worker 共用（§5.3）。
// 默认进程内 4/16，生产通过 pkg/semaphore.RedisSemaphore 提供跨进程全局配额
// （W-F，SETNX+TTL 租约，TTL 覆盖最长执行；崩溃后 TTL 过期自动释放）。
const (
	maxConcurrentBuilds = 4
	maxConcurrentRuns   = 16
)

type CreateExecutionCommand struct {
	ProjectID    string
	FunctionID   string
	DeploymentID string // 缺省用最新 ready deployment
	Data         string
	Async        bool
}

// queueMessage 是入队 payload：execution_id + function_id + project_id + data。
// （schema 无输入数据列，data 必须随队列传递；另含 project-scoped 访问所需 ID，
// 见实现方案 §5.5 偏差说明。）
// attempt 是 worker 重试计数（B2/R07-P3-8）：每次瞬时失败重抛回队前 +1，
// 队列消息本身是唯一事实来源（跨重启/多 worker 副本正确）；旧消息无此字段
// 时 json.Unmarshal 默认容忍，视为 0。
type queueMessage struct {
	ExecutionID string `json:"execution_id"`
	FunctionID  string `json:"function_id"`
	ProjectID   string `json:"project_id"`
	Data        string `json:"data,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
}

func (f *Functions) CreateExecution(ctx context.Context, cmd CreateExecutionCommand) (*domainfunctions.ExecutionRecord, error) {
	// 纵深防御（G2-1/R06-P0，G12 调整）：执行创建允许 admin 会话与 API key。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	fn, err := f.repo.GetFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	if !fn.Enabled {
		return nil, status.Error(codes.FailedPrecondition, "function is disabled")
	}

	dep, err := f.selectDeployment(ctx, cmd.ProjectID, cmd.FunctionID, cmd.DeploymentID)
	if err != nil {
		return nil, err
	}

	if len(cmd.Data) > maxExecutionDataBytes {
		return nil, status.Errorf(codes.InvalidArgument, "data exceeds maximum size of %d bytes", maxExecutionDataBytes)
	}
	// data 必须是 JSON object（R07-P3-7）：数组/标量/字面量 null 一律拒绝——
	// 执行体以 JSON object 语义读取 TW_DATA，非 object 会导致运行时解析异常。
	if cmd.Data != "" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(cmd.Data), &obj); err != nil || obj == nil {
			return nil, status.Error(codes.InvalidArgument, "data must be a JSON object")
		}
	}
	vars, err := f.repo.GetVariables(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if envSize(vars) > maxEnvBytes {
		return nil, status.Errorf(codes.InvalidArgument, "environment variables exceed maximum total size of %d bytes", maxEnvBytes)
	}
	if envSize(vars)+len(cmd.Data) > maxEnvBytes {
		return nil, status.Errorf(codes.InvalidArgument, "data and environment variables exceed combined maximum of %d bytes", maxEnvBytes)
	}

	if !cmd.Async && fn.TimeoutSeconds > maxSyncTimeoutSeconds {
		return nil, status.Errorf(codes.InvalidArgument, "timeout_seconds exceeds %d for synchronous execution, use async", maxSyncTimeoutSeconds)
	}

	now := time.Now()
	rec := &domainfunctions.ExecutionRecord{
		ID:           idgen.UUID().String(),
		FunctionID:   cmd.FunctionID,
		ProjectID:    cmd.ProjectID,
		DeploymentID: dep.ID,
		Status:       domainfunctions.ExecutionStatusQueued,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := f.repo.CreateExecution(ctx, rec); err != nil {
		return nil, err
	}

	if cmd.Async {
		payload, err := json.Marshal(queueMessage{
			ExecutionID: rec.ID,
			FunctionID:  rec.FunctionID,
			ProjectID:   rec.ProjectID,
			Data:        cmd.Data,
		})
		if err != nil {
			return nil, err
		}
		if err := f.queue.Enqueue(ctx, shared.QueueFunctionsExecutions, payload); err != nil {
			// 入队失败：记录标记 failed 并返回错误。
			rec.Status = domainfunctions.ExecutionStatusFailed
			rec.Error = "enqueue failed"
			rec.UpdatedAt = time.Now()
			_ = f.repo.UpdateExecution(ctx, rec)
			return nil, status.Errorf(codes.Unavailable, "enqueue execution: %v", err)
		}
	} else {
		rec, err = f.runExecution(ctx, fn, rec, vars, cmd.Data)
		if err != nil {
			return nil, err
		}
	}

	// 保留策略：清理该函数超过最近 100 条的更旧记录（失败仅记日志）。
	_ = f.repo.PruneOldExecutionsInProject(ctx, cmd.ProjectID, cmd.FunctionID, pruneKeepRecent)
	return rec, nil
}

// selectDeployment 选定部署：显式指定（必须 ready）或最新 ready。
func (f *Functions) selectDeployment(ctx context.Context, projectID, functionID, deploymentID string) (*domainfunctions.Deployment, error) {
	if deploymentID != "" {
		dep, err := f.repo.GetDeployment(ctx, projectID, functionID, deploymentID)
		if err != nil {
			return nil, err
		}
		if dep == nil {
			return nil, status.Error(codes.NotFound, "deployment not found")
		}
		if dep.Status != domainfunctions.DeploymentStatusReady {
			return nil, status.Error(codes.FailedPrecondition, "deployment is not ready")
		}
		return dep, nil
	}
	deps, err := f.repo.ListDeployments(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	// ListDeployments 按 created_at DESC，第一个 ready 即最新。
	for i := range deps {
		if deps[i].Status == domainfunctions.DeploymentStatusReady {
			return &deps[i], nil
		}
	}
	return nil, status.Error(codes.FailedPrecondition, "no ready deployment")
}

// runExecution 同步执行：占执行信号量 → executor → 写回结果（含截断）。
// 超时等执行错误会写回 failed 记录并返回错误（映射 DeadlineExceeded/HTTP 504）。
func (f *Functions) runExecution(ctx context.Context, fn *domainfunctions.Function, rec *domainfunctions.ExecutionRecord, vars map[string]string, data string) (*domainfunctions.ExecutionRecord, error) {
	ok, release, err := f.getRunSemaphore().TryAcquire(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "acquire run semaphore: %v", err)
	}
	if !ok {
		rec.Status = domainfunctions.ExecutionStatusFailed
		rec.Error = "too many concurrent executions"
		rec.UpdatedAt = time.Now()
		_ = f.repo.UpdateExecution(ctx, rec)
		return nil, status.Error(codes.ResourceExhausted, "too many concurrent executions")
	}
	defer release()

	result, err := f.executor.Execute(ctx, buildExecution(fn, rec, vars, data))
	now := time.Now()
	rec.UpdatedAt = now
	if err != nil {
		rec.Status = domainfunctions.ExecutionStatusFailed
		rec.Error = truncate(executionErrorMessage(err), maxOutputBytes)
		if errors.Is(err, context.DeadlineExceeded) {
			rec.Error = "execution timed out"
		}
		if result != nil {
			rec.DurationMS = result.DurationMS
			rec.Stdout, rec.StdoutTruncated = truncateWithFlag(result.Stdout, maxOutputBytes)
			rec.Stderr, rec.StderrTruncated = truncateWithFlag(result.Stderr, maxOutputBytes)
		}
		_ = f.repo.UpdateExecution(ctx, rec)
		f.meterDuration(ctx, rec.ProjectID, rec.DurationMS)
		return rec, err
	}
	rec.StatusCode = result.StatusCode
	rec.DurationMS = result.DurationMS
	rec.Stdout, rec.StdoutTruncated = truncateWithFlag(result.Stdout, maxOutputBytes)
	rec.Stderr, rec.StderrTruncated = truncateWithFlag(result.Stderr, maxOutputBytes)
	rec.Response, rec.ResponseTruncated = truncateWithFlag(result.Response, maxOutputBytes)
	if result.StatusCode != 0 {
		rec.Status = domainfunctions.ExecutionStatusFailed
		if strings.TrimSpace(result.Stderr) != "" {
			rec.Error = truncate(strings.TrimSpace(result.Stderr), maxOutputBytes)
		}
	} else {
		rec.Status = domainfunctions.ExecutionStatusCompleted
	}
	if err := f.repo.UpdateExecution(ctx, rec); err != nil {
		return nil, err
	}
	f.meterDuration(ctx, rec.ProjectID, rec.DurationMS)
	return rec, nil
}

// ProcessExecution 供 worker 消费队列任务：CAS 领取（queued→building，防
// 重复投递并发执行）→ 加载部署 → 补构建（deployment 非 ready）→ running →
// 执行 → 写回。记录已被删除时静默忽略；领取失败（已被其他消费者领取或已
// 处于终态）同样静默跳过——队列 at-least-once 语义下的重复消息在此收敛为
// 单次执行。
func (f *Functions) ProcessExecution(ctx context.Context, msg queueMessage) error {
	rec, err := f.repo.GetExecution(ctx, msg.ProjectID, msg.FunctionID, msg.ExecutionID)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	claimed, err := f.repo.TransitionExecutionStatus(ctx, msg.ProjectID, msg.FunctionID, msg.ExecutionID,
		domainfunctions.ExecutionStatusQueued, domainfunctions.ExecutionStatusBuilding)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	rec.Status = domainfunctions.ExecutionStatusBuilding
	rec.UpdatedAt = time.Now()

	// release 把执行归还回队列（building→queued），供可重试失败后 worker
	// requeue 重投再次领取；归还失败时记录停留在 building，由
	// RecoverOrphanExecutions 在 1h 后标记 failed（与 worker 崩溃孤儿同一兜底）。
	release := func() {
		_, _ = f.repo.TransitionExecutionStatus(context.WithoutCancel(ctx), msg.ProjectID, msg.FunctionID, msg.ExecutionID,
			domainfunctions.ExecutionStatusBuilding, domainfunctions.ExecutionStatusQueued)
	}

	fn, err := f.repo.GetFunction(ctx, msg.ProjectID, msg.FunctionID)
	if err != nil {
		release()
		return err
	}
	if fn == nil {
		// 函数已删除：终态失败（此前静默返回会让记录永久滞留 queued）。
		rec.Status = domainfunctions.ExecutionStatusFailed
		rec.Error = "function not found"
		rec.UpdatedAt = time.Now()
		_ = f.repo.UpdateExecution(ctx, rec)
		return nil
	}
	dep, err := f.repo.GetDeployment(ctx, msg.ProjectID, msg.FunctionID, rec.DeploymentID)
	if err != nil {
		release()
		return err
	}
	if dep == nil {
		rec.Status = domainfunctions.ExecutionStatusFailed
		rec.Error = "deployment not found"
		rec.UpdatedAt = time.Now()
		_ = f.repo.UpdateExecution(ctx, rec)
		return nil
	}

	vars, err := f.repo.GetVariables(ctx, msg.ProjectID, msg.FunctionID)
	if err != nil {
		release()
		return err
	}

	// deployment 非 ready 先补构建（5 分钟超时，防挂死的 daemon 卡住消费）。
	// 构建失败（含信号量满）按可重试处理：归还 queued 并返回错误，由 worker
	// requeue 在退避后重试；重试超限走 failPayload 兜底。
	if dep.Status != domainfunctions.DeploymentStatusReady {
		if err := f.repo.UpdateExecution(ctx, rec); err != nil {
			release()
			return err
		}
		buildCtx, cancel := context.WithTimeout(ctx, workerRebuildTimeout)
		buildErr := f.buildDeployment(buildCtx, dep, zipPath(msg.ProjectID, msg.FunctionID, dep.ID))
		cancel()
		if buildErr != nil {
			release()
			return buildErr
		}
	}

	rec.Status = domainfunctions.ExecutionStatusRunning
	rec.UpdatedAt = time.Now()
	if err := f.repo.UpdateExecution(ctx, rec); err != nil {
		return err
	}

	// 执行超时=fn.TimeoutSeconds；超时写回 failed（不把 DeadlineExceeded 上抛，
	// worker 单任务失败不影响消费循环）。
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(fn.TimeoutSeconds)*time.Second)
	defer cancel()
	ok, release, err := f.getRunSemaphore().TryAcquire(ctx)
	if err != nil {
		return err
	}
	if !ok {
		rec.Status = domainfunctions.ExecutionStatusFailed
		rec.Error = "too many concurrent executions"
		rec.UpdatedAt = time.Now()
		_ = f.repo.UpdateExecution(ctx, rec)
		return nil
	}
	defer release()

	result, err := f.executor.Execute(runCtx, buildExecution(fn, rec, vars, msg.Data))
	now := time.Now()
	rec.UpdatedAt = now
	if err != nil {
		rec.Status = domainfunctions.ExecutionStatusFailed
		if errors.Is(err, context.DeadlineExceeded) {
			rec.Error = "execution timed out"
		} else {
			rec.Error = truncate(executionErrorMessage(err), maxOutputBytes)
		}
		if result != nil {
			rec.DurationMS = result.DurationMS
			rec.Stdout, rec.StdoutTruncated = truncateWithFlag(result.Stdout, maxOutputBytes)
			rec.Stderr, rec.StderrTruncated = truncateWithFlag(result.Stderr, maxOutputBytes)
		}
		_ = f.repo.UpdateExecution(ctx, rec)
		f.meterDuration(ctx, rec.ProjectID, rec.DurationMS)
		return nil
	}
	rec.StatusCode = result.StatusCode
	rec.DurationMS = result.DurationMS
	rec.Stdout, rec.StdoutTruncated = truncateWithFlag(result.Stdout, maxOutputBytes)
	rec.Stderr, rec.StderrTruncated = truncateWithFlag(result.Stderr, maxOutputBytes)
	rec.Response, rec.ResponseTruncated = truncateWithFlag(result.Response, maxOutputBytes)
	if result.StatusCode != 0 {
		rec.Status = domainfunctions.ExecutionStatusFailed
		if strings.TrimSpace(result.Stderr) != "" {
			rec.Error = truncate(strings.TrimSpace(result.Stderr), maxOutputBytes)
		}
	} else {
		rec.Status = domainfunctions.ExecutionStatusCompleted
	}
	if err := f.repo.UpdateExecution(ctx, rec); err != nil {
		return err
	}
	f.meterDuration(ctx, rec.ProjectID, rec.DurationMS)
	_ = f.repo.PruneOldExecutionsInProject(ctx, msg.ProjectID, msg.FunctionID, pruneKeepRecent)
	return nil
}

// meterDuration 把函数执行时长写入当前小时 Redis bucket（best-effort）。
func (f *Functions) meterDuration(ctx context.Context, projectID string, durationMS int64) {
	if f.usage == nil || projectID == "" || durationMS <= 0 {
		return
	}
	meterCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
	defer cancel()
	_ = f.usage.Incr(meterCtx, projectID, domainbilling.MetricFunctionDurationMS, durationMS)
}

func (f *Functions) GetExecution(ctx context.Context, projectID, functionID, executionID string) (*domainfunctions.ExecutionRecord, error) {
	rec, err := f.repo.GetExecution(ctx, projectID, functionID, executionID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, status.Error(codes.NotFound, "execution not found")
	}
	return rec, nil
}

// ProcessExecutionPayload 解析队列 payload 并执行（worker 消费入口）。
func (f *Functions) ProcessExecutionPayload(ctx context.Context, payload []byte) error {
	var msg queueMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidQueuePayload, err)
	}
	if msg.ExecutionID == "" || msg.FunctionID == "" || msg.ProjectID == "" {
		return fmt.Errorf("%w: missing execution/function/project id", ErrInvalidQueuePayload)
	}
	return f.ProcessExecution(ctx, msg)
}

// MarkExecutionFailed 供 worker 在消费重试超限后兜底标记执行失败。
// 仅作用于未终态记录：completed/failed 不被覆盖（防重复投递回写覆盖终态）。
func (f *Functions) MarkExecutionFailed(ctx context.Context, projectID, functionID, executionID, reason string) error {
	return f.repo.FailExecutionIfActive(ctx, projectID, functionID, executionID, reason)
}

// RecoverOrphanExecutions 按 public.projects 枚举 active 项目，将停留
// building/running 超过 staleAfter 的记录标记为 failed（worker 启动对账）。
// 全局预算 recoverOrphanBatch（K22），foreach 项目扣减 remaining；项目遍历
// 按轮转游标起始（队尾饥饿防护）。
func (f *Functions) RecoverOrphanExecutions(ctx context.Context, staleAfter time.Duration) (int64, error) {
	if f.projects == nil {
		return 0, nil
	}
	all, err := f.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	n := len(all)
	start := f.scanCursor.Start(n)
	olderThan := time.Now().Add(-staleAfter)
	remaining := recoverOrphanBatch
	var recovered int64
	stopped := -1
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if remaining <= 0 {
			stopped = idx
			break
		}
		if all[idx].Status != "active" {
			continue
		}
		c, err := f.repo.RecoverOrphanExecutionsInProject(ctx, all[idx].ID, olderThan, remaining)
		if err != nil {
			continue
		}
		recovered += c
		remaining -= int(c)
	}
	if stopped >= 0 {
		f.scanCursor.ResumeAt(stopped)
	} else {
		f.scanCursor.Complete()
	}
	return recovered, nil
}

func (f *Functions) ListExecutions(ctx context.Context, projectID, functionID string) ([]domainfunctions.ExecutionRecord, error) {
	if _, err := f.repo.GetFunction(ctx, projectID, functionID); err != nil {
		return nil, err
	}
	return f.repo.ListExecutions(ctx, projectID, functionID, pruneKeepRecent)
}

// buildExecution 组装 executor 入参。
func buildExecution(fn *domainfunctions.Function, rec *domainfunctions.ExecutionRecord, vars map[string]string, data string) domainfunctions.Execution {
	return domainfunctions.Execution{
		FunctionID:   fn.ID,
		DeploymentID: rec.DeploymentID,
		ProjectID:    fn.ProjectID, // per-project 执行网络寻址（Round4 J5-4）
		Runtime:      fn.Runtime,
		Spec:         fn.Spec,
		Timeout:      int64(fn.TimeoutSeconds),
		Env:          sanitizeEnv(vars),
		Data:         data,
	}
}

func envSize(vars map[string]string) int {
	total := 0
	for k, v := range vars {
		total += len(k) + len(v)
	}
	return total
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func truncateWithFlag(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	return s[:limit], true
}

func executionErrorMessage(err error) string {
	st, ok := status.FromError(err)
	if ok && st.Message() != "" {
		return st.Message()
	}
	return err.Error()
}
