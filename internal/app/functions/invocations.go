package functions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 调用限制（v4 §4：请求 body 上限沿 handler 入口 413 校验；响应 ≤1MiB 在
// Invoker 侧收口）。
const (
	maxInvocationDataBytes = 32 << 10 // data ≤ 32KB（客户端/server 面）
	// maxTriggerDataBytes 是触发器路径 data（封套含 body）绝对上限：1MB body
	// 双编码 ≈ 2.4MB，取 4MB 覆盖。
	maxTriggerDataBytes = 4 << 20
	pruneKeepRecent     = 100 // 保留策略：每函数最多保留最近 100 条
	recoverOrphanBatch  = 500 // worker 启动对账全局预算（K22）
	// pruneTriggerRetention 是 client/http/cron 来源调用记录的时间窗保留
	//（P2 Q7 保留分级）：48h ≥ 2× 最长限频窗口（day=24h），保证限频 DB 降级
	// 路径在最长窗口内的计数行不被 prune 裁掉（少计超发，设计 §5 交互修正）。
	pruneTriggerRetention = 48 * time.Hour
)

// 执行身份（P0）：token 随 X-Tw-Execution-Token 头注入（传输与载荷分离，
// v4 §4）。executionTokenGrace 是 token TTL 的宽限余量：TTL = 函数超时 +
// 60s，仅作崩溃兜底（正常路径调用结束即主动吊销）。
const (
	executionTokenGrace = 60 * time.Second
	// executionTokenRevokeTimeout 是调用结束后主动吊销的独立超时：吊销不得
	// 继承已取消/超时的调用 ctx，也不能无超时阻塞调用链。
	executionTokenRevokeTimeout = 5 * time.Second
	// maxOutputBytes 是 response/error 截断上限（落库口径）。
	maxOutputBytes = 64 << 10
)

// ErrInvalidQueuePayload 标识无法解析或缺失 ID 的队列消息（worker 不应重试）。
var ErrInvalidQueuePayload = errors.New("invalid queue payload")

// InvokeCommand 是一次函数调用命令（v4 §2：CreateExecutionCommand 改名）。
type InvokeCommand struct {
	ProjectID  string
	FunctionID string
	Data       string
	Async      bool
	// ——触发器路径扩展（P1；Server 面 Invoke 不设置）——
	// Source 是触发来源（trigger_source 列）：http:{trigger_id} /
	// cron:{trigger_id} / event:{trigger_id} / client；空 = server 面。
	// INSERT 期写入、之后不可变。
	Source string
	// SourceIP 是 HTTP 触发的来源 IP 摘要（source_ip 列）。
	SourceIP string
	// DataLimitBytes 覆盖默认 32KB data 上限（InvokeTrigger 专用：触发器封套
	// 通道放宽至 ≤4MB 覆盖 1MB body 双编码）。0 = 默认。客户端调用面（P2）
	// 不放宽：严格 32KB。
	DataLimitBytes int
	// ——HTTP 触发器封套通道（v4 §4.1 http.request params；Server 面不设置）——
	// TriggerEnvelope 携带封套元数据（method/path/raw_query/headers 白名单，
	// 不含 body），RawBody 是触发器原始 body（已过 handler 入口 413 校验）。
	// 非 nil = 该调用走 http.request method（fetch 语义）。
	TriggerEnvelope *domainfunctions.TriggerEnvelope
	RawBody         []byte
	// ——客户端调用面扩展（P2；设计 §4）——
	// InvokingUserID 是调用用户（调用记录 invoking_user_id 列；执行身份
	// token info 携带 invoking_user → 账本 operator 溯源 function+user）。
	InvokingUserID string
	// IdempotencyKey 是客户端幂等键（调用记录 client_idempotency_key 列；
	// partial 唯一索引 (project, function, user, key) WHERE key <> '' 去重，
	// 冲突时调用方回读既有行原样返回）。空 = 不参与幂等。
	IdempotencyKey string
	// ——事件轴投递元数据（v4 §4.2，D10；cron/event 触发器路径填充）——
	// EventAxis 非 nil 时该调用走事件轴（Notifier.Notify，CloudEvents）。
	// CloudEvent 的 type/time/data 投影在 app 层组装（worker 只传递原料）。
	EventAxis *domainfunctions.EventNotificationMeta
}

// effectiveDataLimit 计算本次调用的 data 上限：默认 32KB；触发器路径放宽到
// 调用方上限（封套含 body，≤4MB 覆盖）。
func (f *Functions) effectiveDataLimit(override int) int {
	if override <= 0 {
		return maxInvocationDataBytes
	}
	if override > maxTriggerDataBytes {
		return maxTriggerDataBytes
	}
	return override
}

// queueMessage 是入队 payload：invocation_id + function_id + project_id +
// data。（schema 无输入数据列，data 必须随队列传递；另含 project-scoped
// 访问所需 ID。）attempt 是 worker 重试计数（B2/R07-P3-8）：每次瞬时失败
// 重抛回队前 +1，队列消息本身是唯一事实来源（跨重启/多 worker 副本正确）；
// 旧消息无此字段时 json.Unmarshal 默认容忍，视为 0。TriggerEnvelope/RawBody
// 是 http.request 通道（async_ack 路径的封套元数据与原始 body 随队列透传，
// RawBody []byte 经 JSON 自动 base64 往返）；EventAxis 是事件轴投递元数据
// （cron/event 触发器路径，CloudEvent 组装在 app 层——D10）。
type queueMessage struct {
	InvocationID string `json:"invocation_id"`
	FunctionID   string `json:"function_id"`
	ProjectID    string `json:"project_id"`
	Data         string `json:"data,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	// Source 是触发来源（trigger_source 列原值）：worker 双轴分流的依据
	//（cron:/event: 前缀走事件轴 Notifier，其余走 RPC 轴 Invoker——D10）。
	Source          string                                 `json:"source,omitempty"`
	TriggerEnvelope *domainfunctions.TriggerEnvelope       `json:"trigger_envelope,omitempty"`
	RawBody         []byte                                 `json:"raw_body,omitempty"`
	EventAxis       *domainfunctions.EventNotificationMeta `json:"event_axis,omitempty"`
}

// Invoke 执行一次 server 面调用（v4 §2：CreateExecution 改名——一个动作一个
// 名字，与 client 面同名对齐）。
func (f *Functions) Invoke(ctx context.Context, cmd InvokeCommand) (*domainfunctions.InvocationRecord, error) {
	// 纵深防御（G2-1/R06-P0，G12 调整）：调用创建允许 admin 会话与 API key。
	// 触发器路径（InvokeTrigger）token 门禁在 handler 完成，不经此处。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	return f.invokeInternal(ctx, cmd)
}

// invokeInternal 是调用创建核心（Server 面与触发器/客户端路径共用）：两写
// 预占同步快路径 / 异步队列状态机、执行身份铸造、双轴分流（D10）、观测
// 指标全部一致。
func (f *Functions) invokeInternal(ctx context.Context, cmd InvokeCommand) (*domainfunctions.InvocationRecord, error) {
	// 热路径清账（P0.5）：GetFunction 走 30s 进程内缓存。
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
	// endpoint target 是 v4 唯一执行位置（D8'）：缺失（存量行/未配置）按
	// fail-closed 拒绝——无 target 的函数不可调用。
	if fn.TargetURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "function has no target endpoint")
	}

	dataLimit := f.effectiveDataLimit(cmd.DataLimitBytes)
	if len(cmd.Data) > dataLimit {
		return nil, status.Errorf(codes.InvalidArgument, "data exceeds maximum size of %d bytes", dataLimit)
	}
	// RawBody 通道防御（v3 §2.3 沿革）：触发器原始 body 的 413 判定在 handler
	// 入口（MaxTriggerBodyLimit，已过校验）；此处只防异常调用方直调 app 面
	// ——上限与封套 data 同口径。
	if cmd.TriggerEnvelope != nil && len(cmd.RawBody) > maxTriggerDataBytes {
		return nil, status.Errorf(codes.InvalidArgument, "raw body exceeds maximum size of %d bytes", maxTriggerDataBytes)
	}
	// data 必须是 JSON object（R07-P3-7）：数组/标量/字面量 null 一律拒绝——
	// 函数体以 JSON object 语义读取载荷，非 object 会导致运行时解析异常。
	if cmd.Data != "" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(cmd.Data), &obj); err != nil || obj == nil {
			return nil, status.Error(codes.InvalidArgument, "data must be a JSON object")
		}
	}

	if !cmd.Async && fn.TimeoutSeconds > maxSyncTimeoutSeconds {
		return nil, status.Errorf(codes.InvalidArgument, "timeout_seconds exceeds %d for synchronous invocation, use async", maxSyncTimeoutSeconds)
	}

	now := f.nowFn()
	rec := &domainfunctions.InvocationRecord{
		ID:                   idgen.UUID().String(),
		FunctionID:           cmd.FunctionID,
		ProjectID:            cmd.ProjectID,
		TriggerSource:        cmd.Source,
		SourceIP:             cmd.SourceIP,
		InvokingUserID:       cmd.InvokingUserID,
		ClientIdempotencyKey: cmd.IdempotencyKey,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	if cmd.Async {
		// 异步路径状态机（v4 §7 收敛后）：queued 入队 → worker CAS 领取
		//（queued→running）→ 双轴执行 → 终态。
		rec.Status = domainfunctions.InvocationStatusQueued
		if err := f.repo.CreateInvocation(ctx, rec); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(queueMessage{
			InvocationID:    rec.ID,
			FunctionID:      rec.FunctionID,
			ProjectID:       rec.ProjectID,
			Data:            cmd.Data,
			Source:          cmd.Source,
			TriggerEnvelope: cmd.TriggerEnvelope,
			RawBody:         cmd.RawBody,
			EventAxis:       cmd.EventAxis,
		})
		if err != nil {
			return nil, err
		}
		if err := f.queue.Enqueue(ctx, shared.QueueFunctionsInvocations, payload); err != nil {
			// 入队失败：记录标记 failed 并返回错误。
			rec.Status = domainfunctions.InvocationStatusFailed
			rec.Error = "enqueue failed"
			rec.UpdatedAt = f.nowFn()
			_ = f.repo.UpdateInvocation(ctx, rec)
			return nil, status.Errorf(codes.Unavailable, "enqueue invocation: %v", err)
		}
		return rec, nil
	}

	// 同步快路径两写预占记账（P0.5，§6 约束①/K9）：跳过 queued 中间态，
	// INSERT (status=running, timeout_seconds 快照) 直接预占——预占行即刻
	// 成为审计/限频计数依据；并发/重复由调用方幂等（P2 客户端幂等键）与本
	// 行无关；调用中崩溃行留在 running，由周期孤儿恢复按
	// staleAfter = timeout_seconds + 120s 判 failed。
	rec.Status = domainfunctions.InvocationStatusRunning
	rec.TimeoutSeconds = fn.TimeoutSeconds
	if err := f.repo.CreateInvocation(ctx, rec); err != nil {
		return nil, err
	}
	return f.runInvocation(ctx, fn, rec, cmd.Data, cmd.TriggerEnvelope, cmd.RawBody, cmd.EventAxis)
}

// runInvocation 双轴执行（v4 §4，D10；两写预占的第二写）：按 trigger_source
// 分流——client/server/http: 走 RPC 轴（Invoker.Invoke，method=invoke 或
// http.request）；cron:/event: 走事件轴（Notifier.Notify，CloudEvents）。
// 执行结束（成功/失败/panic）主动吊销 token；超时等调用错误会写回 failed
// 记录并返回错误（映射 DeadlineExceeded/HTTP 504）。
//
// 调用并发不设全局信号量：目标恒为常驻服务（D8'），容量归 substrate。
func (f *Functions) runInvocation(ctx context.Context, fn *domainfunctions.Function, rec *domainfunctions.InvocationRecord, data string, triggerEnvelope *domainfunctions.TriggerEnvelope, rawBody []byte, eventAxis *domainfunctions.EventNotificationMeta) (*domainfunctions.InvocationRecord, error) {
	started := f.nowFn()
	// 执行身份（P0）：铸造短期 token 随签名请求头注入（函数回访平台写数据
	// 的凭证通道——「货币/库存只能云函数写」在两轴的凭证）；defer 覆盖成功/
	// 失败/panic 三条路径的主动吊销（TTL 只是崩溃兜底）——常驻的是服务不是
	// 凭证。
	token := f.mintExecutionToken(ctx, fn, rec)
	defer f.revokeExecutionToken(token)

	// 签名密钥解密（secretbox 密文 → 明文；每次调用现场计算 HMAC——实施
	// 勘误 2026-10-15）。解密失败 = 密钥材料配置错误：当次调用按平台失败
	// 返回（可恢复——修复配置后重试即恢复）。
	current, previous, serr := f.decryptTargetSecrets(fn)
	if serr != nil {
		return f.failInvocation(ctx, rec, serr)
	}

	var (
		result *domainfunctions.InvocationResult
		nres   domainfunctions.NotificationResult
		err    error
	)
	if isEventAxisSource(rec.TriggerSource) {
		// ——事件轴（D10）：CloudEvents 1.0 structured mode 通知——
		nres, err = f.notifyEventAxis(ctx, fn, rec, data, eventAxis, current, previous, token)
	} else {
		// ——RPC 轴（D1）：JSON-RPC 2.0 over 签名 HTTP——
		result, err = f.invoker.Invoke(ctx, f.buildInvocation(fn, rec, data, triggerEnvelope, rawBody, current, previous, token))
	}

	now := f.nowFn()
	rec.UpdatedAt = now
	if err != nil {
		return f.failInvocation(ctx, rec, err)
	}
	rec.DurationMS = invocationDuration(result, nres)
	if result != nil {
		rec.StatusCode = result.StatusCode
		// v4 fetch 语义透传字段（运行期字段、不落库——bun 映射显式排除）。
		rec.HTTPHeaders = result.Headers
		rec.ResponseB64 = result.ResponseB64
		rec.Response, rec.ResponseTruncated = truncateWithFlag(result.Response, maxOutputBytes)
		// 函数应用层错误（JSON-RPC error 对象）是合法结果：调用送达成功、
		// 函数报错——记录标 failed（错误文本 = error 对象消息），返回值仍带
		// 记录（调用方区分「函数报错」与「平台失败」看 err 是否为 nil——
		// err==nil 时调用方按 200+failed 语义消费记录）。
		if result.FunctionError != nil {
			rec.Status = domainfunctions.InvocationStatusFailed
			rec.Error = truncate(result.FunctionError.Message, maxOutputBytes)
			if uerr := f.repo.UpdateInvocation(ctx, rec); uerr != nil {
				return nil, uerr
			}
			f.meterDuration(ctx, rec.ProjectID, rec.DurationMS)
			f.observeInvocation(rec.ProjectID, rec.FunctionID, metricSource(rec.TriggerSource), started, rec.Status)
			return rec, nil
		}
	} else {
		// 事件轴（D10 语义损失入册）：投递行不记录函数返回值，响应尾部
		// 截断留档作审计替代。
		rec.Response, rec.ResponseTruncated = truncateWithFlag(nres.ResponseTail, maxOutputBytes)
	}
	// err==nil 且 StatusCode 非 0 仅见于 http.request 的函数 HTTP status
	//——自定义状态码是一等结果，不映射调用失败（D10）；失败已由 error 对象
	// 承载。
	rec.Status = domainfunctions.InvocationStatusCompleted
	if err := f.repo.UpdateInvocation(ctx, rec); err != nil {
		return nil, err
	}
	f.meterDuration(ctx, rec.ProjectID, rec.DurationMS)
	f.observeInvocation(rec.ProjectID, rec.FunctionID, metricSource(rec.TriggerSource), started, rec.Status)
	return rec, nil
}

// failInvocation 平台语义失败的终态写回（failed + 错误文本截断；超时归一
// 固定文案）并上抛原错误。落库失败时以落库错误优先上抛（调用方需要知道
// 记录可能滞留 running，孤儿恢复会兜底）。
func (f *Functions) failInvocation(ctx context.Context, rec *domainfunctions.InvocationRecord, callErr error) (*domainfunctions.InvocationRecord, error) {
	rec.Status = domainfunctions.InvocationStatusFailed
	rec.Error = truncate(invocationErrorMessage(callErr), maxOutputBytes)
	if errors.Is(callErr, context.DeadlineExceeded) || status.Code(callErr) == codes.DeadlineExceeded {
		rec.Error = "invocation timed out"
	}
	if uerr := f.repo.UpdateInvocation(ctx, rec); uerr != nil {
		return nil, uerr
	}
	f.meterDuration(ctx, rec.ProjectID, rec.DurationMS)
	f.observeInvocation(rec.ProjectID, rec.FunctionID, metricSource(rec.TriggerSource), f.nowFn(), rec.Status)
	return rec, callErr
}

// buildInvocation 组装 RPC 轴 Invoker 入参（v4 §4.1）：method 词表分流——
// http.request（封套 params）或 invoke（函数载荷）；id = invocation id
// （请求关联，服务方回显）；身份投影随头注入（Source 空 = server 面）。
func (f *Functions) buildInvocation(fn *domainfunctions.Function, rec *domainfunctions.InvocationRecord, data string, triggerEnvelope *domainfunctions.TriggerEnvelope, rawBody []byte, current, previous, token string) domainfunctions.Invocation {
	method := domainfunctions.MethodInvoke
	if triggerEnvelope != nil {
		method = domainfunctions.MethodHTTPRequest
	}
	return domainfunctions.Invocation{
		FunctionID:           fn.ID,
		ProjectID:            fn.ProjectID,
		TargetURL:            fn.TargetURL,
		TargetSecret:         current,
		TargetPreviousSecret: previous,
		ExecutionToken:       token,
		Method:               method,
		Data:                 data,
		TriggerEnvelope:      triggerEnvelope,
		RawBody:              rawBody,
		TimeoutSeconds:       int64(fn.TimeoutSeconds),
		ExecutionID:          rec.ID,
		Source:               runnerSource(rec.TriggerSource),
		InvokingUserID:       rec.InvokingUserID,
	}
}

// notifyEventAxis 组装并投递事件轴通知（v4 §4.2，D10）：cron → type=
// cron.tick + CronTick data；event → type=事件目录名 + 事件投影 data。
// source=projects/{p}/triggers/{t}（cron 触发器）或 projects/{p}（事件）；
// twproject 恒带；twseq 事件面有 seq 则带。
func (f *Functions) notifyEventAxis(ctx context.Context, fn *domainfunctions.Function, rec *domainfunctions.InvocationRecord, data string, eventAxis *domainfunctions.EventNotificationMeta, current, previous, token string) (domainfunctions.NotificationResult, error) {
	evt, err := f.buildCloudEvent(fn, rec, data, eventAxis)
	if err != nil {
		return domainfunctions.NotificationResult{}, err
	}
	return f.notifier.Notify(ctx, domainfunctions.Notification{
		TargetURL:            fn.TargetURL,
		TargetSecret:         current,
		TargetPreviousSecret: previous,
		Event:                *evt,
		TimeoutSeconds:       int64(fn.TimeoutSeconds),
		ExecutionToken:       token,
	})
}

// buildCloudEvent 组装事件轴信封（v4 §4.2；webhooks buildCloudEvent 同构，
// 函数面差异：id = invocation id、cron 源 source 带 triggers 段、data =
// CronTick 或事件投影——调用方已把投影放在 data 通道）。
func (f *Functions) buildCloudEvent(fn *domainfunctions.Function, rec *domainfunctions.InvocationRecord, data string, eventAxis *domainfunctions.EventNotificationMeta) (*domainfunctions.CloudEvent, error) {
	evt := &domainfunctions.CloudEvent{
		Specversion:     domainfunctions.CloudEventsSpecVersion,
		ID:              rec.ID,
		Type:            eventAxis.Type,
		Time:            eventAxis.Time.UTC().Format(time.RFC3339),
		DataContentType: "application/json",
		TwProject:       fn.ProjectID,
		TwSeq:           eventAxis.Seq,
	}
	if isCronSource(rec.TriggerSource) {
		evt.Source = "projects/" + fn.ProjectID + "/triggers/" + cronTriggerID(rec.TriggerSource)
	} else {
		evt.Source = "projects/" + fn.ProjectID
	}
	if data != "" {
		if !json.Valid([]byte(data)) {
			return nil, status.Errorf(codes.Internal, "event axis data is not valid JSON")
		}
		evt.Data = json.RawMessage(data)
	}
	return evt, nil
}

// ProcessInvocation 供 worker 消费队列任务：CAS 领取（queued→running，防
// 重复投递并发执行）→ 双轴执行 → 写回。记录已被删除时静默忽略；领取失败
// （已被其他消费者领取或已处于终态）同样静默跳过——队列 at-least-once 语义
// 下的重复消息在此收敛为单次执行。可重试失败（DB 暂态）走归还 + worker
// requeue 语义；调用失败（平台/函数语义）为终态 failed、worker 不重试。
func (f *Functions) ProcessInvocation(ctx context.Context, msg queueMessage) error {
	rec, err := f.repo.GetInvocation(ctx, msg.ProjectID, msg.FunctionID, msg.InvocationID)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	claimed, err := f.repo.TransitionInvocationStatus(ctx, msg.ProjectID, msg.FunctionID, msg.InvocationID,
		domainfunctions.InvocationStatusQueued, domainfunctions.InvocationStatusRunning)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	rec.Status = domainfunctions.InvocationStatusRunning
	rec.UpdatedAt = f.nowFn()

	// release 把调用归还回队列（running→queued），供可重试失败后 worker
	// requeue 重投再次领取；归还失败时记录停留在 running，由
	// RecoverOrphanInvocations 在超时宽限后标记 failed（与 worker 崩溃孤儿
	// 同一兜底）。
	release := func() {
		_, _ = f.repo.TransitionInvocationStatus(context.WithoutCancel(ctx), msg.ProjectID, msg.FunctionID, msg.InvocationID,
			domainfunctions.InvocationStatusRunning, domainfunctions.InvocationStatusQueued)
	}

	// 热路径清账（P0.5）：GetFunction 走 30s 进程内缓存（worker 进程同享
	// 失效语义——跨进程 30s 收敛）。
	fn, err := f.getCachedFunction(ctx, msg.ProjectID, msg.FunctionID)
	if err != nil {
		release()
		return err
	}
	if fn == nil {
		// 函数已删除：终态失败（此前静默返回会让记录永久滞留 queued）。
		rec.Status = domainfunctions.InvocationStatusFailed
		rec.Error = "function not found"
		rec.UpdatedAt = f.nowFn()
		_ = f.repo.UpdateInvocation(ctx, rec)
		return nil
	}
	if fn.TargetURL == "" {
		rec.Status = domainfunctions.InvocationStatusFailed
		rec.Error = "function has no target endpoint"
		rec.UpdatedAt = f.nowFn()
		_ = f.repo.UpdateInvocation(ctx, rec)
		return nil
	}

	claimedAt := f.nowFn()

	// 排队时长观测（P0.5 观测补全）：queued→running 的耗时（异步路径）。
	observeQueueWait(msg.ProjectID, msg.FunctionID, claimedAt.Sub(rec.CreatedAt))

	// 调用超时=fn.TimeoutSeconds；超时写回 failed（不把 DeadlineExceeded 上抛，
	// worker 单任务失败不影响消费循环）。
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(fn.TimeoutSeconds)*time.Second)
	defer cancel()

	rec, err = f.runInvocation(runCtx, fn, rec, msg.Data, msg.TriggerEnvelope, msg.RawBody, msg.EventAxis)
	if err != nil {
		// 调用失败（平台/函数语义）已终态写回 failed：不把错误上抛，worker
		// 单任务失败不影响消费循环（与旧 ProcessExecution 失败分支同语义）。
		return nil
	}
	_ = f.repo.PruneOldInvocationsInProject(ctx, msg.ProjectID, msg.FunctionID, pruneKeepRecent)
	_ = rec
	return nil
}

// meterDuration 把函数调用时长写入当前小时 Redis bucket（best-effort）。
// function_duration_ms 语义 = 网关往返时长（v4 §7：Invoker/Notifier 侧计时）。
func (f *Functions) meterDuration(ctx context.Context, projectID string, durationMS int64) {
	if f.usage == nil || projectID == "" || durationMS <= 0 {
		return
	}
	meterCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
	defer cancel()
	_ = f.usage.Incr(meterCtx, projectID, domainbilling.MetricFunctionDurationMS, durationMS)
}

func (f *Functions) GetInvocation(ctx context.Context, projectID, functionID, invocationID string) (*domainfunctions.InvocationRecord, error) {
	rec, err := f.repo.GetInvocation(ctx, projectID, functionID, invocationID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, status.Error(codes.NotFound, "invocation not found")
	}
	return rec, nil
}

// ProcessInvocationPayload 解析队列 payload 并执行（worker 消费入口）。
func (f *Functions) ProcessInvocationPayload(ctx context.Context, payload []byte) error {
	var msg queueMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidQueuePayload, err)
	}
	if msg.InvocationID == "" || msg.FunctionID == "" || msg.ProjectID == "" {
		return fmt.Errorf("%w: missing invocation/function/project id", ErrInvalidQueuePayload)
	}
	return f.ProcessInvocation(ctx, msg)
}

// MarkInvocationFailed 供 worker 在消费重试超限后兜底标记调用失败。
// 仅作用于未终态记录：completed/failed 不被覆盖（防重复投递回写覆盖终态）。
func (f *Functions) MarkInvocationFailed(ctx context.Context, projectID, functionID, invocationID, reason string) error {
	return f.repo.FailInvocationIfActive(ctx, projectID, functionID, invocationID, reason)
}

// RecoverOrphanInvocations 按 public.projects 枚举 active 项目，将停留未终态
// （queued/running）超过 staleAfter 的记录标记为 failed。由 worker 周期
// ticker（1min）驱动；staleAfter 按行内 timeout_seconds 快照 + 120s 宽限
// 判定（repo 侧 SQL 同源；迁移 000029 起列为 NOT NULL，无 NULL 回退）。
// 全局预算 recoverOrphanBatch（K22），foreach 项目扣减 remaining；项目遍历
// 按轮转游标起始（队尾饥饿防护）。
func (f *Functions) RecoverOrphanInvocations(ctx context.Context) (int64, error) {
	if f.projects == nil {
		return 0, nil
	}
	all, err := f.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	n := len(all)
	start := f.scanCursor.Start(n)
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
		c, err := f.repo.RecoverOrphanInvocationsInProject(ctx, all[idx].ID, remaining)
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

// ListInvocations 返回调用记录列表（排障面：created_at DESC，offset 型分页 +
// status/created_at 结构化过滤；返回 next_page_token，空串 = 没有更多页）。
// pruneKeepRecent 只约束保留策略（PruneOldInvocationsInProject），不再充当
// 列表上限。
func (f *Functions) ListInvocations(ctx context.Context, projectID, functionID string, pageSize int, pageToken string, flt domainfunctions.InvocationListFilter) ([]domainfunctions.InvocationRecord, string, error) {
	if flt.Status != "" && !isValidInvocationListStatus(flt.Status) {
		return nil, "", status.Errorf(codes.InvalidArgument, "unknown invocation status %q", flt.Status)
	}
	if _, err := f.repo.GetFunction(ctx, projectID, functionID); err != nil {
		return nil, "", err
	}
	offset := 0
	if pageToken != "" {
		off, err := crud.DecodePageToken(pageToken)
		if err != nil {
			return nil, "", status.Error(codes.InvalidArgument, "invalid page token")
		}
		offset = off
	}
	rows, total, err := f.repo.ListInvocations(ctx, projectID, functionID, pageSize, offset, flt)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > 0 && offset+len(rows) < total {
		tok, err := crud.EncodePageToken(offset + len(rows))
		if err != nil {
			return nil, "", err
		}
		next = tok
	}
	return rows, next, nil
}

// isValidInvocationListStatus 限定调用列表 status 过滤取值（与状态机一致；
// building 态随补构建退役）。
func isValidInvocationListStatus(s string) bool {
	switch s {
	case domainfunctions.InvocationStatusQueued,
		domainfunctions.InvocationStatusRunning,
		domainfunctions.InvocationStatusCompleted,
		domainfunctions.InvocationStatusFailed:
		return true
	}
	return false
}

// runnerSource 把调用行 trigger_source（http:{id} / cron:{id} / event:{id} /
// client / 空 = server 面）投影为协议头 X-Tw-Source 的取值：空值映射为
// "server" 字面值（函数侧 ctx.source 恒非空，handler 无需判空区分）。
// 非空值（含 trigger 前缀）原样透传。
func runnerSource(triggerSource string) string {
	if triggerSource == "" {
		return executionSourceServer
	}
	return triggerSource
}

// isEventAxisSource 报告触发来源是否走事件轴（v4 §4 D10 判定：cron/event
// 触发器 = 通知语义，无调用方在等应用层结果）。
func isEventAxisSource(triggerSource string) bool {
	return isCronSource(triggerSource) || isEventSource(triggerSource)
}

func isCronSource(triggerSource string) bool {
	const p = domainfunctions.TriggerTypeCron + ":"
	return len(triggerSource) > len(p) && triggerSource[:len(p)] == p
}

func isEventSource(triggerSource string) bool {
	const p = domainfunctions.TriggerTypeEvent + ":"
	return len(triggerSource) > len(p) && triggerSource[:len(p)] == p
}

// cronTriggerID 从 "cron:{id}" 形态的 trigger_source 提取触发器 ID（isCronSource
// 已保证形态）。
func cronTriggerID(triggerSource string) string {
	const p = domainfunctions.TriggerTypeCron + ":"
	return triggerSource[len(p):]
}

// invocationDuration 取双轴的网关往返时长（RPC 轴在 InvocationResult，事件
// 轴在 NotificationResult）。
func invocationDuration(result *domainfunctions.InvocationResult, nres domainfunctions.NotificationResult) int64 {
	if result != nil {
		return result.DurationMS
	}
	return nres.DurationMS
}

// mintExecutionToken 铸造本次调用的短期 token（P0 执行身份）。TTL = 函数
// 超时 + 60s 宽限（崩溃兜底）；铸造失败 best-effort 不阻断调用——返回空
// token 即不注入 X-Tw-Execution-Token，函数内平台调用将以 401 呈现（与
// declared_scopes 为空的默认语义一致），故障仅记告警日志。
func (f *Functions) mintExecutionToken(ctx context.Context, fn *domainfunctions.Function, rec *domainfunctions.InvocationRecord) string {
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
			"project_id", fn.ProjectID, "function_id", fn.ID, "invocation_id", rec.ID, "error", err)
		return ""
	}
	return token
}

// revokeExecutionToken 在调用结束（成功/失败/panic——调用方以 defer 挂载）
// 后主动吊销 token：「执行结束即失效」是主动语义。独立 context（Background
// + 短超时）：调用 ctx 可能已取消/超时，吊销不得连带失败或无限阻塞。
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

func invocationErrorMessage(err error) string {
	st, ok := status.FromError(err)
	if ok && st.Message() != "" {
		return st.Message()
	}
	return err.Error()
}
