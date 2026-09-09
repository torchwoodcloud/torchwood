package functions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// 客户端调用面（P2，设计 §4/§5）：Client API（END_USER）按 per-function 策略
// 调用函数。链路 = RequireEndUser → 策略门（enabled / client_callable /
// 匿名一期拒绝）→ 每用户限频（可配窗口，Redis 固定窗口 + DB 降级）→ 每用户
// 并发闸门 → 既有参数化 createExecution（Source="client"、invoking_user_id、
// 幂等键、data 严格 32KB）。执行身份铸造（P0）与账本 operator 溯源
// （function+user）在既有链路内自动生效。
//
// 审计豁免（设计 §6 约束③）：本入口的审计载体是 function_executions 行
// （含 invoking_user_id），该 RPC 在 AuditInterceptor 跳过清单内。
const (
	// clientQuotaKeyPrefix 是限频 Redis 键前缀（设计 §5）：
	// torchwood:fnq:{project}:{function}:{user}:{bucket}。
	clientQuotaKeyPrefix = "torchwood:fnq:"
	// defaultPerUserConcurrency 是每用户并发闸门默认上限（config 可覆盖）。
	defaultPerUserConcurrency = 2
	// defaultUserQueueHeadTimeout 是并发闸门排队队首默认超时。
	defaultUserQueueHeadTimeout = 5 * time.Second
	// quotaReason 是配额超额错误的 ErrorInfo.Reason。
	quotaReason = "FUNCTIONS.INVOKE_QUOTA_EXCEEDED"
)

// ClientInvokeCommand 是客户端调用命令。project/user 取自 Principal
// （请求体不携带身份——身份由平台注入是执行身份模型的前提）。
type ClientInvokeCommand struct {
	ProjectID  string
	FunctionID string
	Data       string
	// DeploymentID 可选：缺省用最新 ready deployment。
	DeploymentID string
	// IdempotencyKey 可选：网络超时重试防重复执行。(project, function, user,
	// key) 唯一去重，命中返回既有 execution 原样（running 返回 running）。
	IdempotencyKey string
}

// ClientInvokeResult 是客户端调用结果：Reused=true 表示命中幂等（既有执行
// 原样返回，未重新执行）。
type ClientInvokeResult struct {
	Record *domainfunctions.ExecutionRecord
	Reused bool
}

// ClientInvoke 执行一次客户端调用（同步默认，≤30s 沿用既有路径；长任务走
// Server 面 CreateExecution(async=true)）。
func (f *Functions) ClientInvoke(ctx context.Context, cmd ClientInvokeCommand) (*ClientInvokeResult, error) {
	// 身份门：仅端用户（admin/API key/匿名一律拒绝）。匿名会话洗限频的
	// 对策在一期是「匿名根本进不来」（Q4 拍板：client_anonymous_allowed
	// 字段保留、管理面禁用），后续放开匿名时在此叠加按 IP 计数兜底。
	if err := appshared.RequireEndUser(ctx); err != nil {
		return nil, err
	}
	// 身份门已保证 principal 存在且为端用户（UserID 非空）；ctx 不携带请求体
	// 身份——身份由平台注入是执行身份模型的前提。
	p, _ := contexts.Principal(ctx)
	userID := p.UserID

	fn, err := f.getCachedFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		f.observeClientInvoke(cmd, InvokeResultNotFound)
		return nil, status.Error(codes.NotFound, "function not found")
	}
	// enabled 门沿用既有 disabled 语义（createExecution 同型错误）。
	if !fn.Enabled {
		return nil, status.Error(codes.FailedPrecondition, "function is disabled")
	}
	// 策略门（存量全 FALSE ⇒ fail-closed）：非 client_callable →
	// PermissionDenied（这不是 CreateExecution 的放开，是带独立策略门的新入口）。
	if !fn.ClientCallable {
		f.observeClientInvoke(cmd, InvokeResultError)
		return nil, status.Error(codes.PermissionDenied, "function is not client-callable")
	}

	// 每用户限频（可配窗口）：先限频后执行（先 INCR 后预占，与通用限流同
	// 原子性）。limit=0 理论上不可达（client_callable=true 要求 limit≥1），
	// 防御性跳过。
	if fn.ClientPerUserLimit > 0 {
		start, end := clientQuotaWindowBounds(fn.ClientLimitWindow, time.Now())
		if err := f.checkClientQuota(ctx, cmd, fn, userID, start, end); err != nil {
			return nil, err
		}
	}

	// 每用户并发闸门（P2，设计 §5）：防单用户挤占执行槽（噪声邻居是开放
	// 客户端触发面的头号风险）。全局 run 信号量/池上限之外的第二道门。
	release, err := f.userGate.acquire(ctx, userID)
	if err != nil {
		f.observeClientInvoke(cmd, InvokeResultQuota)
		return nil, err
	}
	defer release()

	rec, err := f.createExecution(ctx, CreateExecutionCommand{
		ProjectID:      cmd.ProjectID,
		FunctionID:     cmd.FunctionID,
		DeploymentID:   cmd.DeploymentID,
		Data:           cmd.Data,
		Source:         domainfunctions.TriggerSourceClient,
		InvokingUserID: userID,
		IdempotencyKey: cmd.IdempotencyKey,
		// data 上限不放宽：客户端面严格 32KB（与 Server 面一致；触发器的
		// 4MB 放宽是封套通道专用，不适用不可信调用方）。
	})
	if err != nil {
		// 幂等命中：并发同键第二请求预占 INSERT 冲突 → 回读既有行原样返回
		// （running 返回 running；设计 §4「重复键在执行进行中返回既有记录，
		// 完成则原样返回」）。
		if cmd.IdempotencyKey != "" && errors.Is(err, domainfunctions.ErrExecutionIdempotencyConflict) {
			existing, gerr := f.repo.GetExecutionByIdempotencyKey(ctx, cmd.ProjectID, cmd.FunctionID, userID, cmd.IdempotencyKey)
			if gerr == nil && existing != nil {
				f.observeClientInvoke(cmd, InvokeResultOK)
				return &ClientInvokeResult{Record: existing, Reused: true}, nil
			}
			// 冲突行回读失败（极端：刚插入即被删）按原始错误继续上抛语义
			// 归一为 Internal——不吞错。
			return nil, status.Error(codes.Internal, "idempotency conflict but existing execution unavailable")
		}
		f.observeClientInvokeCmd(cmd, err)
		return nil, err
	}
	f.observeClientInvoke(cmd, InvokeResultOK)
	return &ClientInvokeResult{Record: rec, Reused: false}, nil
}

// checkClientQuota 判定每用户限频：Redis 固定窗口优先；Redis 故障降级为
// function_executions 窗口内计数（trigger_source='client' AND invoking_user_id
// AND created_at >= 窗口起点——依赖 Q7 保留分级先落地，48h 时间窗保证计数行
// 在最长窗口内不被条数 prune 裁掉）；DB 亦不可用则拒绝（fail-closed，与
// 配额哲学对齐——经济语义的限频不做通用限流式 fail-open 熔断）。
func (f *Functions) checkClientQuota(ctx context.Context, cmd ClientInvokeCommand, fn *domainfunctions.Function, userID string, windowStart, windowEnd time.Time) error {
	if f.clientQuota != nil {
		key := clientQuotaKey(cmd.ProjectID, cmd.FunctionID, userID, fn.ClientLimitWindow, windowStart)
		res, err := f.clientQuota.Allow(ctx, key, fn.ClientPerUserLimit, windowEnd)
		if err == nil {
			if res.Allowed {
				return nil
			}
			return quotaExceededError(windowEnd)
		}
		// Redis 故障：落 DB 降级（记指标通道外另行告警由日志承载）。
	}
	n, err := f.repo.CountClientInvocations(ctx, cmd.ProjectID, cmd.FunctionID, userID, windowStart)
	if err != nil {
		// DB 亦不可用：fail-closed。
		return status.Error(codes.Unavailable, "client invoke quota check unavailable")
	}
	if n >= fn.ClientPerUserLimit {
		return quotaExceededError(windowEnd)
	}
	return nil
}

// quotaExceededError 构造配额超额错误：ResourceExhausted + ErrorInfo.Reason
// （FUNCTIONS.INVOKE_QUOTA_EXCEEDED）+ RetryInfo（窗口结束时刻）。配额超额
// 不是响应字段（设计 §4）。
func quotaExceededError(windowEnd time.Time) error {
	st := status.New(codes.ResourceExhausted, "client invoke quota exceeded for this window")
	st, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   quotaReason,
		Domain:   "torchwood.platform",
		Metadata: map[string]string{"retryable": "true"},
	})
	if err != nil {
		return status.Error(codes.ResourceExhausted, "client invoke quota exceeded for this window")
	}
	if !windowEnd.IsZero() {
		if delay := time.Until(windowEnd); delay > 0 {
			if st, err = st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(delay)}); err != nil {
				return status.Error(codes.ResourceExhausted, "client invoke quota exceeded for this window")
			}
		}
	}
	return st.Err()
}

// clientQuotaWindowBounds 计算限频窗口的 [起点, 结束) 与 bucket（设计 §5）：
// minute=YYYYMMDDHHMM、hour=YYYYMMDDHH、day=UTC YYYYMMDD。未知值按 day
// （DB CHECK 已挡，防御性回退）。
func clientQuotaWindowBounds(window string, now time.Time) (time.Time, time.Time) {
	utc := now.UTC()
	switch window {
	case domainfunctions.ClientLimitWindowMinute:
		start := utc.Truncate(time.Minute)
		return start, start.Add(time.Minute)
	case domainfunctions.ClientLimitWindowHour:
		start := utc.Truncate(time.Hour)
		return start, start.Add(time.Hour)
	default:
		start := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
		return start, start.Add(24 * time.Hour)
	}
}

// clientQuotaKey 组装限频 Redis 键：torchwood:fnq:{project}:{function}:{user}:{bucket}。
func clientQuotaKey(projectID, functionID, userID, window string, windowStart time.Time) string {
	utc := windowStart.UTC()
	var bucket string
	switch window {
	case domainfunctions.ClientLimitWindowMinute:
		bucket = utc.Format("200601021504")
	case domainfunctions.ClientLimitWindowHour:
		bucket = utc.Format("2006010215")
	default:
		bucket = utc.Format("20060102")
	}
	return fmt.Sprintf("%s%s:%s:%s:%s", clientQuotaKeyPrefix, projectID, functionID, userID, bucket)
}

// observeClientInvoke 记录一次客户端调用入口计数（best-effort；client 维度
// 复用 ObserveInvoke 的 result 词表）。
func (f *Functions) observeClientInvoke(cmd ClientInvokeCommand, result string) {
	ObserveInvoke(cmd.ProjectID, cmd.FunctionID, executionSourceClient, result)
}

// observeClientInvokeCmd 按错误形态折叠 result 词表（timeout 与 error 分列，
// 与触发器 handler 口径一致）。
func (f *Functions) observeClientInvokeCmd(cmd ClientInvokeCommand, err error) {
	result := InvokeResultError
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		result = InvokeResultTimeout
	}
	f.observeClientInvoke(cmd, result)
}

// parseDurationValue 解析时长配置字符串；空/非法回落默认值。
func parseDurationValue(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// ——每用户并发闸门（进程内 keyed 信号量，P2 设计 §5）——
//
// 语义说明（诚实声明）：这是 per-process 信号量，多实例部署下为「近似全局」
// ——每实例各持有 per-user 上限（默认 2），全局并发的理论上限 = 上限 × 实例数。
// 跨进程精确全局闸门需要 Redis 分布式信号量，但排队 + 队首超时的同步等待
// 语义在 Redis 实现下是每请求一次往返轮询（或 pub/sub），对 ≤100ms 热路径
// 不划算；进程内实现 + 文档明示是本期的工程取舍。

// userGateLimiter 是每用户并发闸门：每个用户一个固定容量 channel，acquire
// 即带队首超时的 channel 接收（排队语义天然成立），release 归还槽位。
type userGateLimiter struct {
	mu      sync.Mutex
	limit   int
	timeout time.Duration
	slots   map[string]chan struct{}
}

func newUserGateLimiter(limit int, queueHeadTimeout time.Duration) *userGateLimiter {
	if limit <= 0 {
		limit = defaultPerUserConcurrency
	}
	if queueHeadTimeout <= 0 {
		queueHeadTimeout = defaultUserQueueHeadTimeout
	}
	return &userGateLimiter{limit: limit, timeout: queueHeadTimeout, slots: map[string]chan struct{}{}}
}

// acquire 取一个该用户的执行槽；满额时排队（FIFO 由 channel 语义保证），
// 队首超时 → ResourceExhausted。返回的 release 必须调用（通常 defer）。
func (g *userGateLimiter) acquire(ctx context.Context, userID string) (func(), error) {
	ch := g.slot(userID)
	timer := time.NewTimer(g.timeout)
	defer timer.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-timer.C:
		return nil, status.Errorf(codes.ResourceExhausted,
			"per-user concurrency limit reached (max %d per user)", g.limit)
	case <-ctx.Done():
		return nil, status.Error(codes.Canceled, ctx.Err().Error())
	}
}

// slot 返回该用户的槽位 channel（按需创建；常驻不删——channel 为 8 字节
// 结构，按用户数线性，量级可忽略，删除的竞态不值得）。
func (g *userGateLimiter) slot(userID string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.slots[userID]
	if !ok {
		ch = make(chan struct{}, g.limit)
		g.slots[userID] = ch
	}
	return ch
}
