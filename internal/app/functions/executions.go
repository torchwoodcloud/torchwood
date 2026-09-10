package functions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 执行限制（§5.3 / §9.6）。
const (
	maxExecutionDataBytes = 32 << 10 // data ≤ 32KB（execve 单变量硬限制余量）
	maxEnvBytes           = 32 << 10 // env vars 总量 ≤ 32KB
	maxOutputBytes        = 64 << 10 // stdout/stderr/response 截断上限
	// maxTriggerDataBytes 是触发器路径 data（封套）绝对上限（v2 dispatcher
	// body 通道）：封套同时携带 body（best-effort UTF-8 字符串）与
	// body_base64（无损），1MB body 双编码 ≈ 2.4MB，取 4MB 覆盖。
	maxTriggerDataBytes = 4 << 20
	pruneKeepRecent     = 100 // 保留策略：每函数最多保留最近 100 条
	recoverOrphanBatch  = 500 // worker 启动对账全局预算（K22）
	// pruneTriggerRetention 是 client/http/cron 来源执行记录的时间窗保留
	// （P2 Q7 保留分级）：48h ≥ 2× 最长限频窗口（day=24h），保证限频 DB 降级
	// 路径在最长窗口内的计数行不被 prune 裁掉（少计超发，设计 §5 交互修正）。
	pruneTriggerRetention = 48 * time.Hour
	// workerRebuildTimeout 是 worker 补构建的最长耗时（防挂死的 daemon 卡住消费）。
	workerRebuildTimeout = 5 * time.Minute
)

// 执行身份 env（P0）：注入容器的是 token 原值与 Server API 可达地址。两者
// 计入 env+data ≤32KB 预算（token ≈ 60B、URL 典型 ≤100B，量级无碍；合并
// 预算兜底在 infra/functions docker.go maxExecEnvBudgetBytes）。
const (
	twExecutionTokenEnv = "TW_EXECUTION_TOKEN"
	twAPIBaseURLEnv     = "TW_API_BASE_URL"
	// executionTokenGrace 是 token TTL 的宽限余量：TTL = 函数超时 + 60s，
	// 仅作崩溃兜底（正常路径执行结束即主动吊销，见 revokeExecutionToken）。
	executionTokenGrace = 60 * time.Second
	// executionTokenRevokeTimeout 是执行结束后主动吊销的独立超时：吊销不得
	// 继承已取消/超时的执行 ctx，也不能无超时阻塞调用链。
	executionTokenRevokeTimeout = 5 * time.Second
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
	ProjectID  string
	FunctionID string
	// DeploymentID 缺省用最新 ready deployment。
	DeploymentID string
	Data         string
	Async        bool
	// ——触发器路径扩展（P1；Server 面 CreateExecution 不设置）——
	// Source 是触发来源（trigger_source 列）：http:{trigger_id} /
	// cron:{trigger_id} / client；空 = server 面。INSERT 期写入、之后不可变。
	Source string
	// SourceIP 是 HTTP 触发的来源 IP 摘要（source_ip 列）。
	SourceIP string
	// DataLimitBytes 覆盖默认 32KB data 上限（InvokeTrigger 专用：v2
	// dispatcher 走 body 通道可放宽至触发器 body 上限 ≤1MB；v1 env 通道
	// 保持 32KB 硬上限）。0 = 默认。客户端调用面（P2）不放宽：严格 32KB。
	DataLimitBytes int
	// ——HTTP 触发器封套通道（v3 §2.3/D10；Server 面 CreateExecution 不
	// 设置）——恒填充（app 不探测 runner 风格）：TriggerEnvelope 携带封套
	// 元数据（method/path/raw_query/headers 白名单，不含 body），RawBody 是
	// 触发器原始 body（已过 handler 入口 413 校验）。runner fetch 风格还原
	// Request、main 风格重组 TW_DATA（与现状等价，双轨 D9）。
	TriggerEnvelope *domainfunctions.TriggerEnvelope
	RawBody         []byte
	// ——客户端调用面扩展（P2；设计 §4）——
	// InvokingUserID 是调用用户（执行记录 invoking_user_id 列；执行身份
	// token info 携带 invoking_user → 账本 operator 溯源 function+user）。
	InvokingUserID string
	// IdempotencyKey 是客户端幂等键（执行记录 client_idempotency_key 列；
	// partial 唯一索引 (project, function, user, key) WHERE key <> '' 去重，
	// 冲突时调用方回读既有行原样返回）。空 = 不参与幂等。
	IdempotencyKey string
}

// effectiveDataLimit 计算本次执行的 data 上限：默认 32KB（execve 单变量
// 硬限制余量）；触发器路径在 v2（dispatcher body 通道）下放宽到调用方
// 上限（≤1MB，封套校验），v1（env 通道）保持 32KB。
func (f *Functions) effectiveDataLimit(override int) int {
	if override <= 0 {
		return maxExecutionDataBytes
	}
	if !f.executorV2() {
		return maxExecutionDataBytes
	}
	if override > maxTriggerDataBytes {
		return maxTriggerDataBytes
	}
	return override
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
	// ——HTTP 触发器封套通道（v3 §2.3/D10）：async_ack 路径的封套元数据与
	// 原始 body 随队列透传（worker 执行时同 sync 路径进封套模式）；
	// RawBody []byte 经 JSON 自动 base64 往返。
	TriggerEnvelope *domainfunctions.TriggerEnvelope `json:"trigger_envelope,omitempty"`
	RawBody         []byte                           `json:"raw_body,omitempty"`
}

func (f *Functions) CreateExecution(ctx context.Context, cmd CreateExecutionCommand) (*domainfunctions.ExecutionRecord, error) {
	// 纵深防御（G2-1/R06-P0，G12 调整）：执行创建允许 admin 会话与 API key。
	// 触发器路径（InvokeTrigger）token 门禁在 handler 完成，不经此处。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	return f.createExecution(ctx, cmd)
}

// createExecution 是执行创建核心（Server 面与触发器路径共用）：两写预占
// 同步快路径 / 异步队列状态机、执行身份铸造、观测指标全部一致。
func (f *Functions) createExecution(ctx context.Context, cmd CreateExecutionCommand) (*domainfunctions.ExecutionRecord, error) {
	// 热路径清账（P0.5）：GetFunction/GetVariables 走 30s 进程内缓存。
	fn, err := f.getCachedFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	if !fn.Enabled {
		return nil, status.Error(codes.FailedPrecondition, "function is disabled")
	}

	dep, err := f.selectDeployment(ctx, fn, cmd.DeploymentID)
	if err != nil {
		return nil, err
	}

	dataLimit := f.effectiveDataLimit(cmd.DataLimitBytes)
	if len(cmd.Data) > dataLimit {
		return nil, status.Errorf(codes.InvalidArgument, "data exceeds maximum size of %d bytes", dataLimit)
	}
	// RawBody 通道防御（v3 §2.3）：触发器原始 body 的 413 判定在 handler
	// 入口（MaxTriggerBodyLimit，已过校验）；此处只防异常调用方直调 app 面
	// ——上限与封套 data 同口径。
	if cmd.TriggerEnvelope != nil && len(cmd.RawBody) > maxTriggerDataBytes {
		return nil, status.Errorf(codes.InvalidArgument, "raw body exceeds maximum size of %d bytes", maxTriggerDataBytes)
	}
	// data 必须是 JSON object（R07-P3-7）：数组/标量/字面量 null 一律拒绝——
	// 执行体以 JSON object 语义读取 TW_DATA，非 object 会导致运行时解析异常。
	if cmd.Data != "" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(cmd.Data), &obj); err != nil || obj == nil {
			return nil, status.Error(codes.InvalidArgument, "data must be a JSON object")
		}
	}
	vars, err := f.getCachedVariables(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if envSize(vars) > maxEnvBytes {
		return nil, status.Errorf(codes.InvalidArgument, "environment variables exceed maximum total size of %d bytes", maxEnvBytes)
	}
	// env+data 合并预算只约束 v1 env 通道（TW_DATA 是环境变量）；v2 dispatcher
	// 把 data 走 body 通道与 env 分离，触发器封套（≤1MB）不受 32KB env 预算
	// 连坐（P1 触发器 body 可配至 1MB 的前提）。
	if dataLimit <= maxExecutionDataBytes && envSize(vars)+len(cmd.Data) > maxEnvBytes {
		return nil, status.Errorf(codes.InvalidArgument, "data and environment variables exceed combined maximum of %d bytes", maxEnvBytes)
	}

	if !cmd.Async && fn.TimeoutSeconds > maxSyncTimeoutSeconds {
		return nil, status.Errorf(codes.InvalidArgument, "timeout_seconds exceeds %d for synchronous execution, use async", maxSyncTimeoutSeconds)
	}

	now := time.Now()
	rec := &domainfunctions.ExecutionRecord{
		ID:            idgen.UUID().String(),
		FunctionID:    cmd.FunctionID,
		ProjectID:     cmd.ProjectID,
		DeploymentID:  dep.ID,
		TriggerSource: cmd.Source,
		SourceIP:      cmd.SourceIP,
		// 客户端调用面（P2）：INSERT 期写入、之后不可变。
		InvokingUserID:       cmd.InvokingUserID,
		ClientIdempotencyKey: cmd.IdempotencyKey,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	// egress 分类（P2 安全切片，设计 Security #6）：不可信函数（client_callable
	// 或存在 http/cron 触发器）容器走 internal 变体网络（出网全 deny）。
	// 分类是函数属性，对所有触发来源一致生效。
	untrusted := fn.ClientCallable || f.hasTriggersCached(ctx, cmd.ProjectID, cmd.FunctionID)

	if cmd.Async {
		// 异步路径状态机原样保留：queued 入队 → worker CAS 领取。
		rec.Status = domainfunctions.ExecutionStatusQueued
		if err := f.repo.CreateExecution(ctx, rec); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(queueMessage{
			ExecutionID:     rec.ID,
			FunctionID:      rec.FunctionID,
			ProjectID:       rec.ProjectID,
			Data:            cmd.Data,
			TriggerEnvelope: cmd.TriggerEnvelope,
			RawBody:         cmd.RawBody,
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
		return rec, nil
	}

	// 同步快路径两写预占记账（P0.5，§6 约束①/K9）：跳过 queued/building
	// 中间态，INSERT (status=running, timeout_seconds 快照) 直接预占——
	// 预占行即刻成为审计/限频计数依据；并发/重复由调用方幂等（P2 客户端
	// 幂等键）与本行无关；执行中崩溃行留在 running，由周期孤儿恢复按
	// staleAfter = timeout_seconds + 120s 判 failed。
	timeoutSnapshot := fn.TimeoutSeconds
	rec.Status = domainfunctions.ExecutionStatusRunning
	rec.TimeoutSeconds = &timeoutSnapshot
	if err := f.repo.CreateExecution(ctx, rec); err != nil {
		return nil, err
	}
	return f.runExecution(ctx, fn, rec, dep, vars, cmd.Data, cmd.TriggerEnvelope, cmd.RawBody, untrusted)
}

// selectDeployment 选定部署：显式指定（必须 ready）或最新 ready。
//
// 热路径清账（P0.5，§6 约束③）：优先读函数行的 latest_ready_deployment_id
// 冗余指针（CreateExecution 已取回 fn，指针命中时仅一次 GetDeployment 单行
// 查询）；指针为 NULL（存量数据）或失效（指向行被并发删除/非 ready）时
// 回退既有全量列表逻辑——指针只加速不裁剪正确性。
func (f *Functions) selectDeployment(ctx context.Context, fn *domainfunctions.Function, deploymentID string) (*domainfunctions.Deployment, error) {
	projectID, functionID := fn.ProjectID, fn.ID
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
	if fn.LatestReadyDeploymentID != "" {
		dep, err := f.repo.GetDeployment(ctx, projectID, functionID, fn.LatestReadyDeploymentID)
		if err == nil && dep != nil && dep.Status == domainfunctions.DeploymentStatusReady {
			return dep, nil
		}
		// 指针失效：回退全量列表（下一轮 Activate/Delete 会修复指针）。
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

// runExecution 同步执行（两写预占的第二写）：执行身份铸造 → executor →
// 终态 UPDATE（completed/failed + outputs + duration_ms）。执行结束（成功/
// 失败/panic）主动吊销 token；超时等执行错误会写回 failed 记录并返回错误
// （映射 DeadlineExceeded/HTTP 504）。dep 是 selectDeployment 的结果——
// TemplateVersion 供并发降级判定（v3 §1.5）。triggerEnvelope/rawBody 是
// HTTP 触发器封套通道（v3 §2.3/D10；非触发器调用恒 nil/nil）。
//
// 信号量：v2（dispatcher）路径跳过全局 run 信号量——常驻实例池由
// dispatcher 内部管控（池上限/有界排队），全局 16 槽是 per-execution 预算，
// 双重限流会互相饿死（设计 §6）；v1 回退模式保留信号量。
func (f *Functions) runExecution(ctx context.Context, fn *domainfunctions.Function, rec *domainfunctions.ExecutionRecord, dep *domainfunctions.Deployment, vars map[string]string, data string, triggerEnvelope *domainfunctions.TriggerEnvelope, rawBody []byte, egressUntrusted bool) (*domainfunctions.ExecutionRecord, error) {
	if !f.executorV2() {
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
	}

	started := time.Now()
	// 执行身份（P0）：铸造短期 token 并注入 env（v2 下由 dispatcher 客户端
	// 摘出经分发 header 传递，语义不变）；defer 覆盖成功/失败/panic 三条
	// 路径的主动吊销（TTL 只是崩溃兜底）——常驻的是容器不是凭证。
	token := f.mintExecutionToken(ctx, fn, rec)
	defer f.revokeExecutionToken(token)

	result, err := f.executor.Execute(ctx, f.buildExecution(fn, rec, dep.TemplateVersion, vars, data, token, f.executionAPIBaseURL(), triggerEnvelope, rawBody, egressUntrusted))
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
		f.observeExecution(rec.ProjectID, rec.FunctionID, metricSource(rec.TriggerSource), started, rec.Status)
		return rec, err
	}
	rec.StatusCode = result.StatusCode
	rec.DurationMS = result.DurationMS
	rec.Stdout, rec.StdoutTruncated = truncateWithFlag(result.Stdout, maxOutputBytes)
	rec.Stderr, rec.StderrTruncated = truncateWithFlag(result.Stderr, maxOutputBytes)
	rec.Response, rec.ResponseTruncated = truncateWithFlag(result.Response, maxOutputBytes)
	// v4 fetch 风格透传字段（v3 §2.2/D10）：运行期字段、不落库（bun 映射
	// 显式排除）；main 风格恒空。
	rec.HTTPHeaders = result.Headers
	rec.ResponseB64 = result.ResponseB64
	if result.StatusCode != 0 && !f.executorV2() {
		// v1 退出码语义：非零 = failed。v2 dispatcher 路径失败已由 err 承载
		//（此处 err==nil 且 StatusCode 非 0 仅见于 v4 fetch 风格的函数 HTTP
		// status——自定义状态码是一等结果，不再映射执行失败，D10）。
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
	f.observeExecution(rec.ProjectID, rec.FunctionID, metricSource(rec.TriggerSource), started, rec.Status)
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

	// 热路径清账（P0.5）：GetFunction/GetVariables 走 30s 进程内缓存（worker
	// 进程同享失效语义——跨进程 30s 收敛）。
	fn, err := f.getCachedFunction(ctx, msg.ProjectID, msg.FunctionID)
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

	vars, err := f.getCachedVariables(ctx, msg.ProjectID, msg.FunctionID)
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
	claimedAt := time.Now()

	// 排队时长观测（P0.5 观测补全）：queued→running 的耗时（异步路径；
	// source 枚举位 client/http/cron 随 P1/P2 打开，server 路径不记排队）。
	observeQueueWait(msg.ProjectID, msg.FunctionID, claimedAt.Sub(rec.CreatedAt))

	// 执行超时=fn.TimeoutSeconds；超时写回 failed（不把 DeadlineExceeded 上抛，
	// worker 单任务失败不影响消费循环）。
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(fn.TimeoutSeconds)*time.Second)
	defer cancel()

	// 信号量：v2（dispatcher）路径跳过全局 run 信号量（同 runExecution 注释
	// ——池由 dispatcher 内部管控，避免双重限流）；v1 回退模式保留。
	var runRelease func()
	if !f.executorV2() {
		ok, rel, err := f.getRunSemaphore().TryAcquire(ctx)
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
		runRelease = rel
	}
	if runRelease != nil {
		defer runRelease()
	}

	// 执行身份（P0）：与同步路径同一铸造/注入/主动吊销语义（进程无关）。
	token := f.mintExecutionToken(ctx, fn, rec)
	defer f.revokeExecutionToken(token)

	// egress 分类（P2）：异步路径（http async_ack / cron）的函数按同一分类
	// 规则判定——存在 http/cron 触发器即不可信（与 createExecution 同口径）。
	untrusted := fn.ClientCallable || f.hasTriggersCached(ctx, msg.ProjectID, msg.FunctionID)
	result, err := f.executor.Execute(runCtx, f.buildExecution(fn, rec, dep.TemplateVersion, vars, msg.Data, token, f.executionAPIBaseURL(), msg.TriggerEnvelope, msg.RawBody, untrusted))
	execStart := time.Now()
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
		f.observeExecution(rec.ProjectID, rec.FunctionID, metricSource(rec.TriggerSource), execStart, rec.Status)
		return nil
	}
	rec.StatusCode = result.StatusCode
	rec.DurationMS = result.DurationMS
	rec.Stdout, rec.StdoutTruncated = truncateWithFlag(result.Stdout, maxOutputBytes)
	rec.Stderr, rec.StderrTruncated = truncateWithFlag(result.Stderr, maxOutputBytes)
	rec.Response, rec.ResponseTruncated = truncateWithFlag(result.Response, maxOutputBytes)
	rec.HTTPHeaders = result.Headers
	rec.ResponseB64 = result.ResponseB64
	if result.StatusCode != 0 && !f.executorV2() {
		// 同 runExecution：v1 退出码语义不变；v4 fetch 风格的函数 HTTP
		// status 是一等结果（D10）。
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
	f.observeExecution(rec.ProjectID, rec.FunctionID, metricSource(rec.TriggerSource), execStart, rec.Status)
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

// RecoverOrphanExecutions 按 public.projects 枚举 active 项目，将停留未终态
// （queued/building/running——P0.5 起纳入 queued）超过 staleAfter 的记录
// 标记为 failed。P0.5 起由 worker 周期 ticker（1min）驱动（原「启动跑一次」
// 升级）；staleAfter 是 timeout_seconds 为 NULL 的存量行的回退口径，非空行
// 按行内快照 + 120s 宽限判定（repo 侧 SQL 同源）。全局预算
// recoverOrphanBatch（K22），foreach 项目扣减 remaining；项目遍历按轮转
// 游标起始（队尾饥饿防护）。
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

// buildExecution 组装 executor 入参。池策略（P0.5）从函数记录透传——
// dispatcher 无 DB 依赖，策略随执行规格携带；v1 executor 忽略。
// egressUntrusted 由调用方按函数分类（P2 安全切片：client_callable 或存在
// http/cron 触发器 = 不可信，容器 attach internal 变体网络）。
// depTemplateVersion 是本次执行所用 deployment 的模板版本（0 = v1 模板/
// 未知）：并发降级判定（v3 §1.5「fail-safe 不 fail-closed」）——函数
// concurrency > 1 而模板 < v3（MinConcurrencyTemplateVersion，不支持
// ctx/分桶/per-request 超时）时静默按并发 1 执行 + 指标观测，存量函数不因
// 新列拒绝执行，重部署后自然生效。
// 执行 ID 取自预占 INSERT 已生成的 rec.ID（v3 §1.2：经分发 header
// x-tw-execution-id 透传给 runner 的 ctx.executionId）。
// triggerEnvelope/rawBody 是 HTTP 触发器封套通道（v3 §2.3/D10）：**恒填充**
// （app 不探测 runner 风格——fetch 与否由 runner 入口探测决定）；runner
// fetch 风格还原 Request、main 风格重组 TW_DATA（与现状等价，双轨 D9）。
func (f *Functions) buildExecution(fn *domainfunctions.Function, rec *domainfunctions.ExecutionRecord, depTemplateVersion int32, vars map[string]string, data, execToken, apiBaseURL string, triggerEnvelope *domainfunctions.TriggerEnvelope, rawBody []byte, egressUntrusted bool) domainfunctions.Execution {
	env := sanitizeEnv(vars)
	// 执行身份 env（P0）：为空则不注入对应变量（api_base_url 未配置时函数
	// 需自行解析平台地址；token 为空 = 无平台身份）。v2 路径下 dispatcher
	// 客户端把 TW_EXECUTION_TOKEN 从 env 摘出经分发 header 传递（通道切换，
	// 语义不变）。
	if execToken != "" {
		env[twExecutionTokenEnv] = execToken
	}
	if apiBaseURL != "" {
		env[twAPIBaseURLEnv] = apiBaseURL
	}
	// 并发生效值（v3 §1.5 降级保护）：模板 < v3（MinConcurrencyTemplateVersion）
	// 时静默按 1。判定基准固定在 v3（v4 §2.1 仅扩展接口面、并发语义不变），
	// 不随 RunnerTemplateVersion 漂移——否则 v4 版本 bump 会把存量 v3
	// deployment 误降级。v1 docker executor 无池概念、Concurrency 本就被
	// 忽略（§1.6），降级计数只在 v2 dispatcher 路径记账避免噪音。
	concurrency := fn.Concurrency
	if concurrency > 1 && depTemplateVersion < domainfunctions.MinConcurrencyTemplateVersion {
		concurrency = 1
		if f.executorV2() {
			observeConcurrencyDowngraded(fn.ProjectID, fn.ID)
		}
	}
	return domainfunctions.Execution{
		FunctionID:   fn.ID,
		DeploymentID: rec.DeploymentID,
		ProjectID:    fn.ProjectID, // per-project 执行网络寻址（Round4 J5-4）
		Runtime:      fn.Runtime,
		Spec:         fn.Spec,
		Timeout:      int64(fn.TimeoutSeconds),
		Env:          env,
		Data:         data,
		// 池策略（v2 常驻执行模型）：平台默认 + per-function 覆盖。
		MinInstances:           fn.MinInstances,
		MaxInstances:           fn.MaxInstances,
		IdleTTLSeconds:         fn.IdleTTLSeconds,
		MaxRequestsPerInstance: fn.MaxRequestsPerInstance,
		// 单实例并发（v3 §1.1，降级判定后的生效值）+ 执行 ID 透传。
		Concurrency: concurrency,
		ExecutionID: rec.ID,
		// HTTP 触发器封套通道（v3 §2.3/D10）：非触发器调用恒 nil/nil，
		// dispatcher 不进封套模式。
		TriggerEnvelope: triggerEnvelope,
		RawBody:         rawBody,
		// egress 分类（P2 安全切片）：infra 据此选择常规/internal 网络。
		EgressUntrusted: egressUntrusted,
	}
}

// mintExecutionToken 铸造本次执行的短期 token（P0 执行身份）。TTL = 函数
// 超时 + 60s 宽限（崩溃兜底）；铸造失败 best-effort 不阻断执行——返回空
// token 即不注入 TW_EXECUTION_TOKEN，函数内平台调用将以 401 呈现（与
// declared_scopes 为空的默认语义一致），故障仅记告警日志。
func (f *Functions) mintExecutionToken(ctx context.Context, fn *domainfunctions.Function, rec *domainfunctions.ExecutionRecord) string {
	if f.execTokens == nil {
		return ""
	}
	ttl := time.Duration(fn.TimeoutSeconds)*time.Second + executionTokenGrace
	token, err := f.execTokens.Mint(ctx, domainfunctions.ExecutionTokenInfo{
		ProjectID:      fn.ProjectID,
		FunctionID:     fn.ID,
		ExecutionID:    rec.ID,
		Scopes:         fn.DeclaredScopes,
		InvokingUserID: rec.InvokingUserID,
	}, ttl)
	if err != nil {
		slog.WarnContext(ctx, "mint execution token failed; function runs without platform identity",
			"project_id", fn.ProjectID, "function_id", fn.ID, "execution_id", rec.ID, "error", err)
		return ""
	}
	return token
}

// revokeExecutionToken 在执行结束（成功/失败/panic——调用方以 defer 挂载）
// 后主动吊销 token：「执行结束即失效」是主动语义。独立 context（Background
// + 短超时）：执行 ctx 可能已取消/超时，吊销不得连带失败或无限阻塞。
func (f *Functions) revokeExecutionToken(token string) {
	if token == "" || f.execTokens == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), executionTokenRevokeTimeout)
	defer cancel()
	if err := f.execTokens.Revoke(ctx, token); err != nil {
		slog.Warn("revoke execution token failed", "error", err)
	}
}

// executionAPIBaseURL 读取 functions.execution.api_base_url（函数容器经
// bridge NAT 回访 Server API 的可达地址；空 = 不注入 TW_API_BASE_URL）。
func (f *Functions) executionAPIBaseURL() string {
	if f.cfg == nil {
		return ""
	}
	return f.cfg.GetFunctions().GetExecution().GetApiBaseUrl()
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
