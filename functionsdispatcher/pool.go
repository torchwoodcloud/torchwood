package functionsdispatcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PoolConfig 是池管理器的全局参数（config functions.dispatcher.*；零值取默认）。
type PoolConfig struct {
	MaxResidentInstances int           // 每 daemon 常驻总量上限（默认 8，Q11）
	QueueDepth           int           // 单函数排队深度上限（默认 32）
	QueueHeadTimeout     time.Duration // 队首超时（默认 10s）
	BootTimeout          time.Duration // 启动健康探针上限（默认 60s）
	ReaperInterval       time.Duration // idle/幽灵对账周期（默认 15s）
	PollInterval         time.Duration // 等待空闲实例的轮询间隔（默认 25ms）
	LeaseTTL             time.Duration // dispatch 租约（默认 10min）
	IdleTTLDefault       time.Duration // idle_ttl 缺省值（300s，策略零值时）
	MaxRequestsDefault   int           // max_requests 缺省值（1000）
	MaxInstancesDefault  int           // max_instances 缺省值（2）
	// TimeoutBudget 是实例累计超时熔断阈值（v3 §1.4；默认 5）：超时释放路径
	// 累加 timeouts 计数，达阈值杀实例重建（堵住「超时不杀」打开的僵尸负载
	// 通道——毒化实例慢性塞满事件循环而 health 仍响应、永不回收）。
	TimeoutBudget int
}

// DefaultPoolConfig 返回平台默认池参数（设计 §6 池策略 + Q11 拍板）。
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxResidentInstances: defaultMaxResidentInstances,
		QueueDepth:           defaultQueueDepth,
		QueueHeadTimeout:     defaultQueueHeadTimeout,
		BootTimeout:          defaultBootTimeout,
		ReaperInterval:       defaultReaperInterval,
		PollInterval:         defaultPollInterval,
		LeaseTTL:             leaseTTL,
		IdleTTLDefault:       300 * time.Second,
		MaxRequestsDefault:   1000,
		MaxInstancesDefault:  2,
		TimeoutBudget:        defaultTimeoutBudget,
	}
}

// PoolConfigFromConfig 从 AppConfig 抽取池参数（仅 dispatcher 进程消费）。
func PoolConfigFromConfig(cfg *config.AppConfig) PoolConfig {
	pc := DefaultPoolConfig()
	d := cfg.GetFunctions().GetDispatcher()
	if d == nil {
		return pc
	}
	if d.GetMaxResidentInstances() > 0 {
		pc.MaxResidentInstances = int(d.GetMaxResidentInstances())
	}
	if d.GetQueueDepth() > 0 {
		pc.QueueDepth = int(d.GetQueueDepth())
	}
	if v := d.GetQueueHeadTimeout(); v != "" {
		if dur, err := time.ParseDuration(v); err == nil && dur > 0 {
			pc.QueueHeadTimeout = dur
		}
	}
	if v := d.GetBootTimeout(); v != "" {
		if dur, err := time.ParseDuration(v); err == nil && dur > 0 {
			pc.BootTimeout = dur
		}
	}
	if d.GetTimeoutBudget() > 0 {
		pc.TimeoutBudget = int(d.GetTimeoutBudget())
	}
	return pc
}

// invokeResult 是一次 runner 调用的结果投影（runnerClient 抽象解耦 HTTP，
// 池逻辑可用 fake 全表驱动测试）。
type invokeResult struct {
	// HTTPStatus 是 runner 响应状态码（0 = 传输层失败）。
	HTTPStatus int
	// Ok/Result/Stdout/Stderr/Error 来自响应封套（见 runner 协议）。
	Ok     bool
	Result string
	Stdout string
	Stderr string
	Error  string
	// ——fetch 风格扩展（v3 §2.2；main 风格封套不含这些字段，恒零值）——
	// FnStatus 是函数返回的 HTTP status（恒 ≥200）；Headers/BodyB64 是函数
	// 设置的响应头与无损 body（64KB 截断在 runner 完成）。
	FnStatus int
	Headers  map[string]string
	BodyB64  string
}

// runnerClient 是 runner 容器内 HTTP 协议的抽象。
type runnerClient interface {
	// Health 探针：实例就绪（ready）返回 nil。
	Health(ctx context.Context, ip string) error
	// Invoke 分发一次执行：body = TW_DATA JSON（封套模式 = 触发器原始
	// body，v3 §2.3）；header 带执行 token + execution id（v3 §1.2
	// ctx.executionId 来源，空则不发）+ 函数超时（v3 per-request 超时
	// header，runner 缺省 30s）+ 触发器封套元数据（x-tw-trigger-envelope，
	// TriggerEnvelope 非空时）；超时/连接失败返回 error（调用方据此分类
	// 处置——超时不杀实例，传输错误杀实例，v3 §1.4）。
	Invoke(ctx context.Context, ip string, req ExecuteRequest, timeout time.Duration) (*invokeResult, error)
}

// httpRunner 是 runnerClient 的真实实现。
type httpRunner struct{ hc *http.Client }

func newHTTPRunner() *httpRunner {
	// 整体 Timeout 0：Invoke 用带超时的独立 ctx（每请求超时 = 函数超时）。
	// Transport 复用连接：池内同实例串行分发 keep-alive 命中率高（SLA 预算
	// 的桥网络 HTTP 往返 ≤5ms 依赖此）。
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 3 * time.Second}).DialContext
	return &httpRunner{hc: &http.Client{Transport: tr}}
}

func runnerURL(ip, path string) string {
	return fmt.Sprintf("http://%s:%d%s", ip, runnerPort, path)
}

func (h *httpRunner) Health(ctx context.Context, ip string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runnerURL(ip, "/_tw/health"), nil)
	if err != nil {
		return err
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runner not ready (status %d)", resp.StatusCode)
	}
	return nil
}

// triggerEnvelopeHeader 编码触发器封套元数据为分发 header 值（base64 JSON，
// v3 §2.3 对抗审查修正：封套经独立 header 传递、不含 body；编码后上限
// maxTriggerEnvelopeHeaderBytes，超限 InvalidArgument——Node http 解析默认
// 16KB，base64 膨胀后须显式限界）。Dispatch 入口先行校验，此处防御性复用。
func triggerEnvelopeHeader(env *domainfunctions.TriggerEnvelope) (string, error) {
	meta, err := json.Marshal(env)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "marshal trigger envelope: %v", err)
	}
	enc := base64.StdEncoding.EncodeToString(meta)
	if len(enc) > maxTriggerEnvelopeHeaderBytes {
		return "", status.Errorf(codes.InvalidArgument, "trigger envelope header exceeds %d bytes (max %d)", len(enc), maxTriggerEnvelopeHeaderBytes)
	}
	return enc, nil
}

func (h *httpRunner) Invoke(ctx context.Context, ip string, req ExecuteRequest, timeout time.Duration) (*invokeResult, error) {
	// v3 §2.3 封套模式：HTTP body 改发触发器原始 body（RawBody；base64 形态
	// 先解码），封套元数据走独立 header；忽略 Data（封套 JSON 不再上分发
	// 通道——body_base64 已无损直达，省一层封套解析）。
	body := []byte(req.Data)
	contentType := "application/json"
	var envelopeHeader string
	if req.TriggerEnvelope != nil {
		raw := req.RawBody
		if req.RawBodyIsB64 {
			decoded, err := base64.StdEncoding.DecodeString(string(req.RawBody))
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "raw_body is not valid base64: %v", err)
			}
			raw = decoded
		}
		body = raw
		contentType = ""
		var err error
		if envelopeHeader, err = triggerEnvelopeHeader(req.TriggerEnvelope); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, runnerURL(ip, "/"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req2.Header.Set("Content-Type", contentType)
	}
	if token := req.ExecutionToken; token != "" {
		// 执行身份注入通道（P0.5 切换）：token 经分发 header 传递，runner
		// 侧逐请求同步写入 process.env（同步 main 兼容；并发下 async 函数
		// 必须读 ctx，见 runner v3 文件头注释，v3 §1.2）。
		req2.Header.Set("X-Tw-Execution-Token", token)
	}
	if executionID := req.ExecutionID; executionID != "" {
		// v3 §1.2：平台执行 ID 透传，runner 侧进 ctx.executionId（日志关联）。
		req2.Header.Set("X-Tw-Execution-Id", executionID)
	}
	if envelopeHeader != "" {
		// v3 §2.3：触发器封套元数据（base64 JSON，不含 body）。
		req2.Header.Set("X-Tw-Trigger-Envelope", envelopeHeader)
	}
	// v3 §1.2：per-request 超时随分发 header 下发（runner 按此起定时器，
	// 到点回 500 封套并放弃等待；header 与本 ctx 超时同源同值——ctx 起点
	// 更早，故 dispatcher 侧几乎总是先行超时收场，runner 封套是兜底）。
	if timeout > 0 {
		req2.Header.Set("X-Tw-Timeout-Seconds", strconv.FormatInt(int64(timeout/time.Second), 10))
	}
	resp, err := h.hc.Do(req2)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body2, err := io.ReadAll(io.LimitReader(resp.Body, maxInvokeResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read runner response: %w", err)
	}
	// result 是用户 main 的任意 JSON 返回值（对象/数组/标量），必须用
	// RawMessage 承接：声明为 string 时对象返回值触发 UnmarshalTypeError
	// 且被静默忽略——ok 已部分解析为 true、result 丢失，执行结果静默变空
	//（CI e2e 实证）。Response 对外契约是 string（JSON 文本透传）。
	// status/headers/body_base64/truncated 是 v4 fetch 风格扩展（v3 §2.2）。
	var envelope struct {
		Ok        bool              `json:"ok"`
		Result    json.RawMessage   `json:"result"`
		Stdout    string            `json:"stdout"`
		Stderr    string            `json:"stderr"`
		Error     string            `json:"error"`
		Status    int               `json:"status"`
		Headers   map[string]string `json:"headers"`
		BodyB64   string            `json:"body_base64"`
		Truncated bool              `json:"truncated"`
	}
	_ = json.Unmarshal(body2, &envelope)
	return &invokeResult{
		HTTPStatus: resp.StatusCode,
		Ok:         envelope.Ok,
		Result:     string(envelope.Result),
		Stdout:     envelope.Stdout,
		Stderr:     envelope.Stderr,
		Error:      envelope.Error,
		FnStatus:   envelope.Status,
		Headers:    envelope.Headers,
		BodyB64:    envelope.BodyB64,
	}, nil
}

// PoolManager 是常驻实例池管理器（设计 §6）：冷启动 = 池 0→1 扩容的单一
// 路径；spawn 收敛 / 有界排队 / idle 回收 / drain / 幽灵对账 / 判活均在此。
//
// docker 交互抽象为 Daemon/Registry/runnerClient 接口（fake 表驱动测试）。
type PoolManager struct {
	daemon   Daemon
	registry Registry
	runner   runnerClient
	cfg      PoolConfig

	// clock/sleep 可注入（表驱动测试）；生产用 time.Now/timer。
	clock func() time.Time
	sleep func(context.Context, time.Duration) bool // 返回 false = ctx 已取消

	mu            sync.Mutex
	waiters       map[string]int  // per function 排队深度（有界排队）
	booting       map[string]int  // per function 启动中实例数
	counted       map[string]bool // 已计入 resident 的容器 ID（防重复计数）
	residentTotal int             // 进程内常驻总量（独立于全局 run 信号量，Q11 上限）
	// lastSpawnErr/lastSpawnWarnAt 按函数保留最近一次被吞掉的 spawn 失败：
	// Dispatch 不因 spawn 失败立即失败请求（继续排队等他人 spawn 成果），但
	// 现场必须留存——否则镜像坏档之类的真实根因会被泛化的 queue timeout
	// 完全吞掉（EACCES 秒退事故的排障放大器）。lastSpawnWarnAt 供告警限频。
	lastSpawnErr    map[string]error
	lastSpawnWarnAt map[string]time.Time
}

// NewPoolManager 构造池管理器（生产装配）。
func NewPoolManager(daemon Daemon, registry Registry, cfg PoolConfig) *PoolManager {
	return newPoolManager(daemon, registry, newHTTPRunner(), cfg)
}

func newPoolManager(daemon Daemon, registry Registry, runner runnerClient, cfg PoolConfig) *PoolManager {
	if cfg.MaxResidentInstances <= 0 {
		cfg.MaxResidentInstances = defaultMaxResidentInstances
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = defaultQueueDepth
	}
	cfg.QueueHeadTimeout = normalDur(cfg.QueueHeadTimeout, defaultQueueHeadTimeout)
	cfg.BootTimeout = normalDur(cfg.BootTimeout, defaultBootTimeout)
	cfg.ReaperInterval = normalDur(cfg.ReaperInterval, defaultReaperInterval)
	cfg.PollInterval = normalDur(cfg.PollInterval, defaultPollInterval)
	cfg.LeaseTTL = normalDur(cfg.LeaseTTL, leaseTTL)
	cfg.IdleTTLDefault = normalDur(cfg.IdleTTLDefault, 300*time.Second)
	if cfg.MaxRequestsDefault <= 0 {
		cfg.MaxRequestsDefault = 1000
	}
	if cfg.MaxInstancesDefault <= 0 {
		cfg.MaxInstancesDefault = 2
	}
	if cfg.TimeoutBudget <= 0 {
		cfg.TimeoutBudget = defaultTimeoutBudget
	}
	return &PoolManager{
		daemon:          daemon,
		registry:        registry,
		runner:          runner,
		cfg:             cfg,
		clock:           time.Now,
		sleep:           defaultSleep,
		waiters:         map[string]int{},
		booting:         map[string]int{},
		counted:         map[string]bool{},
		lastSpawnErr:    map[string]error{},
		lastSpawnWarnAt: map[string]time.Time{},
	}
}

func normalDur(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func defaultSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func waiterKey(ref FunctionRef) string { return ref.ProjectID + ":" + ref.FunctionID }

// recordSpawnFailure 记录一次被吞掉的 spawn 失败：保存最近失败原因（队首
// 超时错误携带，第一现场止血）并限频 slog.Warn（排队请求每 PollInterval 重试
// trySpawn，不拦就是刷屏）。
func (p *PoolManager) recordSpawnFailure(key string, req ExecuteRequest, err error) {
	p.mu.Lock()
	p.lastSpawnErr[key] = err
	now := p.clock()
	if last, ok := p.lastSpawnWarnAt[key]; ok && now.Sub(last) < spawnWarnInterval {
		p.mu.Unlock()
		return
	}
	p.lastSpawnWarnAt[key] = now
	p.mu.Unlock()
	slog.Warn("functions-dispatcher: spawn attempt failed; request keeps waiting in queue",
		"project", req.ProjectID, "function", req.FunctionID, "error", err)
}

// lastSpawnFailure 返回该函数最近一次被吞掉的 spawn 失败（无则 nil）。
func (p *PoolManager) lastSpawnFailure(key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSpawnErr[key]
}

// applyDefaults 归一化单请求池策略（调用方零值字段取平台默认）。
func (p *PoolManager) applyDefaults(policy PoolPolicy) PoolPolicy {
	maxInstances := policy.MaxInstances
	if maxInstances <= 0 {
		maxInstances = p.cfg.MaxInstancesDefault
	}
	idleTTL := policy.IdleTTLSeconds
	if idleTTL <= 0 {
		idleTTL = int(p.cfg.IdleTTLDefault.Seconds())
	}
	maxReq := policy.MaxRequestsPerInstance
	if maxReq <= 0 {
		maxReq = p.cfg.MaxRequestsDefault
	}
	minInstances := policy.MinInstances
	if minInstances < 0 {
		minInstances = 0
	}
	if minInstances > maxInstances {
		minInstances = maxInstances
	}
	// v3 切片一：concurrency 恒 1（<=0 归一化；>1 入口在切片二 A2 接
	// DB/proto 透传链后才可达）。
	concurrency := policy.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	return PoolPolicy{
		MinInstances:           minInstances,
		MaxInstances:           maxInstances,
		IdleTTLSeconds:         idleTTL,
		MaxRequestsPerInstance: maxReq,
		Concurrency:            concurrency,
	}
}

// Dispatch 分发一次执行（热路径）：认领空闲实例 → runner HTTP；无空闲则
// 冷启动（池 0→1 扩容，spawn 收敛）或有界排队；超限 ResourceExhausted。
func (p *PoolManager) Dispatch(ctx context.Context, req ExecuteRequest) (*ExecuteResponse, error) {
	if req.ProjectID == "" || req.FunctionID == "" || req.DeploymentID == "" || req.Image == "" {
		return nil, status.Error(codes.InvalidArgument, "project/function/deployment/image are required")
	}
	// v3 §2.3：触发器封套元数据 header 上限（编码后 12KB）在入口校验——
	// 先于实例认领，超限 InvalidArgument 不产生任何实例副作用。
	if req.TriggerEnvelope != nil {
		if _, err := triggerEnvelopeHeader(req.TriggerEnvelope); err != nil {
			return nil, err
		}
	}
	ref := FunctionRef{ProjectID: req.ProjectID, FunctionID: req.FunctionID}
	policy := p.applyDefaults(req.Pool)

	// 有界排队：深度上限 + 队首超时（超限 ResourceExhausted——同步调用方
	// 不得无界等在 30s ctx 上，设计 §6 边界补全）。
	key := waiterKey(ref)
	p.mu.Lock()
	if p.waiters[key] >= p.cfg.QueueDepth {
		p.mu.Unlock()
		DispatchQueueDropped.WithLabelValues(req.ProjectID, req.FunctionID).Inc()
		return nil, status.Errorf(codes.ResourceExhausted, "function execution queue is full (max %d)", p.cfg.QueueDepth)
	}
	p.waiters[key]++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.waiters[key]--
		if p.waiters[key] <= 0 {
			delete(p.waiters, key)
			// 该函数已无等待请求：spawn 失败现场一并清账（防 map 无界增长；
			// 错误已在超时返回时物化进错误消息，无需再保留）。
			delete(p.lastSpawnErr, key)
			delete(p.lastSpawnWarnAt, key)
		}
		p.mu.Unlock()
	}()

	deadline := p.clock().Add(p.cfg.QueueHeadTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	started := p.clock()
	for {
		rec, err := p.registry.ClaimIdle(ctx, ref, req.DeploymentID, p.clock().Add(p.cfg.LeaseTTL))
		if err != nil {
			return nil, err
		}
		if rec != nil {
			return p.executeOn(ctx, req, *rec, started)
		}

		if err := p.trySpawn(ctx, req, policy); err != nil {
			// spawn 失败（daemon 故障等）：不立刻失败整个请求——继续等待
			// 其他请求的 spawn 成果或空闲实例，直至队首超时。ctx 取消例外。
			if errors.Is(err, context.Canceled) || status.Code(err) == codes.ResourceExhausted {
				return nil, err
			}
			// 被吞掉的失败必须留现场：全量告警（限频防刷屏）+ 保存最近失败
			// 原因供队首超时错误携带（EACCES 秒退事故中真实根因在此消失，
			// 只剩泛化 queue timeout，排障多花数轮）。
			p.recordSpawnFailure(key, req, err)
		}

		if !p.clock().Before(deadline) {
			DispatchQueueTimeouts.WithLabelValues(req.ProjectID, req.FunctionID).Inc()
			if lastErr := p.lastSpawnFailure(key); lastErr != nil {
				return nil, status.Errorf(codes.ResourceExhausted,
					"no free instance within queue head timeout (max_instances=%d; last spawn error: %v)",
					policy.MaxInstances, lastErr)
			}
			return nil, status.Errorf(codes.ResourceExhausted,
				"no free instance within queue head timeout (max_instances=%d)", policy.MaxInstances)
		}
		if !p.sleep(ctx, p.cfg.PollInterval) {
			return nil, ctx.Err()
		}
	}
}

// trySpawn 尝试冷启动一个新实例（池 0→1 扩容）：同函数并发 spawn 用 Redis
// SETNX 锁收敛为一次（其余请求等注册表，防 daemon 重启后全量冷启动风暴）；
// 常驻总量受 daemon 级上限约束（独立于全局 run 信号量，Q11）。
func (p *PoolManager) trySpawn(ctx context.Context, req ExecuteRequest, policy PoolPolicy) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ref := FunctionRef{ProjectID: req.ProjectID, FunctionID: req.FunctionID}
	acquired, release, err := p.registry.AcquireSpawnLock(ctx, ref, p.cfg.BootTimeout+p.cfg.QueueHeadTimeout)
	if err != nil {
		return err
	}
	if !acquired {
		return nil // 已有并发 spawn 在途：等待注册表出现新实例
	}
	defer release()

	// 拿到锁后双检（等锁期间池可能已被扩满）。
	records, err := p.registry.List(ctx, ref)
	if err != nil {
		return err
	}
	key := waiterKey(ref)
	p.mu.Lock()
	live := len(records) + p.booting[key]
	if live >= policy.MaxInstances || p.residentTotal >= p.cfg.MaxResidentInstances {
		p.mu.Unlock()
		return nil // 池满/总量满：交给排队路径
	}
	p.booting[key]++
	p.residentTotal++ // 先占额：spawn 失败路径负责回退
	p.mu.Unlock()

	rec, err := p.spawnInstance(ctx, req, policy)
	if err != nil {
		p.mu.Lock()
		p.booting[key]--
		p.residentTotal--
		p.mu.Unlock()
		return err
	}
	p.mu.Lock()
	p.counted[rec.ContainerID] = true
	p.mu.Unlock()

	// 注册表可见后即允许其他等待请求认领；本请求仍参与 ClaimIdle 竞争
	// （谁抢到谁执行，冷启动成本由触发 spawn 的请求支付但不独占实例）。
	if err := p.registry.Save(ctx, ref, *rec); err != nil {
		p.terminate(ctx, rec.ContainerID)
		_ = p.registry.Delete(ctx, ref, rec.InstanceID)
		p.mu.Lock()
		p.booting[key]--
		p.residentTotal--
		delete(p.counted, rec.ContainerID)
		p.mu.Unlock()
		return err
	}
	p.mu.Lock()
	p.booting[key]--
	p.mu.Unlock()
	return nil
}

// spawnInstance 创建 + 启动握手 + 记录构造（返回未认领的 busy=false 记录）。
// 冷启动（池 0→1）计数与初始化时长在此观测。
func (p *PoolManager) spawnInstance(ctx context.Context, req ExecuteRequest, policy PoolPolicy) (*InstanceRecord, error) {
	bootStart := p.clock()
	ColdStartsTotal.WithLabelValues(req.ProjectID, req.FunctionID).Inc()

	network, err := p.daemon.EnsureProjectNetwork(ctx, req.ProjectID, req.EgressUntrusted)
	if err != nil {
		return nil, err
	}
	// egress 分类计数（P2 安全切片；v1 与 v2 的实例创建路径都打点）。
	infrafunctions.ObserveEgressClass(req.ProjectID, req.EgressUntrusted)
	env := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		// v2 语义：TW_DATA 由请求体承载、TW_EXECUTION_TOKEN 经分发 header
		// 传递——都不进容器 env（常驻的是容器不是凭证）。
		if k == "TW_DATA" || k == "TW_EXECUTION_TOKEN" {
			continue
		}
		env = append(env, k+"="+v)
	}
	inst, err := p.daemon.SpawnInstance(ctx, SpawnOptions{
		ProjectID:   req.ProjectID,
		Image:       req.Image,
		Network:     network,
		Env:         env,
		Spec:        req.Spec,
		MaxRequests: policy.MaxRequestsPerInstance,
	})
	if err != nil {
		return nil, err
	}

	// 启动握手：runner 加载用户模块完成后 /_tw/health 才 ready；探针上限
	// BootTimeout（模块加载失败常驻 not-ready → 到点回收并报错）。
	healthy := false
	deadline := p.clock().Add(p.cfg.BootTimeout)
	for {
		hctx, hcancel := context.WithTimeout(ctx, 2*time.Second)
		err := p.runner.Health(hctx, inst.IP)
		hcancel()
		if err == nil {
			healthy = true
			break
		}
		if ctx.Err() != nil || !p.clock().Before(deadline) {
			break
		}
		p.sleep(ctx, 100*time.Millisecond)
	}
	if !healthy {
		p.terminate(ctx, inst.ContainerID)
		return nil, status.Errorf(codes.DeadlineExceeded, "resident instance failed health probe within %s", p.cfg.BootTimeout)
	}

	InitDurationSeconds.WithLabelValues(req.ProjectID, req.FunctionID).Observe(p.clock().Sub(bootStart).Seconds())
	now := p.clock()
	return &InstanceRecord{
		InstanceID:   inst.ContainerID,
		ContainerID:  inst.ContainerID,
		IP:           inst.IP,
		DeploymentID: req.DeploymentID,
		Inflight:     0,
		// 并发上限 spawn 时固化（v3 §1.1「生效时机」）：实例终生按 spawn 时
		// 策略服务，函数调大后存量实例按旧值服务至 idle 回收/部署更替。
		Concurrency:    policy.Concurrency,
		SpawnedAtMS:    now.UnixMilli(),
		IdleSinceMS:    now.UnixMilli(),
		LeaseUntilMS:   now.Add(p.cfg.LeaseTTL).UnixMilli(),
		MinInstances:   policy.MinInstances,
		IdleTTLSeconds: policy.IdleTTLSeconds,
		MaxRequests:    policy.MaxRequestsPerInstance,
	}, nil
}

// executeOn 在已认领实例上分发请求并负责释放/回收。max_requests 判 draining
// 已移入释放 Lua（按记录固化值，v3 §1.3），此处不再消费池策略。
func (p *PoolManager) executeOn(ctx context.Context, req ExecuteRequest, rec InstanceRecord, queuedAt time.Time) (*ExecuteResponse, error) {
	ref := FunctionRef{ProjectID: req.ProjectID, FunctionID: req.FunctionID}
	QueueWaitSeconds.WithLabelValues(req.ProjectID, req.FunctionID).Observe(p.clock().Sub(queuedAt).Seconds())
	started := p.clock()
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	res, invokeErr := p.runner.Invoke(ctx, rec.IP, req, timeout)
	duration := p.clock().Sub(started)
	DispatchDurationSeconds.WithLabelValues(req.ProjectID, req.FunctionID).Observe(duration.Seconds())

	if invokeErr != nil && (ctx.Err() != nil || isTimeoutErr(invokeErr)) {
		// 超时/调用方取消（v3 §1.4 表格）：不杀实例——并发下一个慢请求不得
		// 误杀同实例健康在途请求；走释放 Lua（幂等，timedOut=true 累加熔断
		// 计数），请求照旧以 DeadlineExceeded 收场，runner per-request timer
		// 兜底 abandon。清账用独立 ctx（执行 ctx 可能已死）。
		noCancel := context.WithoutCancel(ctx)
		released, rerr := p.registry.Release(noCancel, ref, rec.InstanceID, p.clock(), p.clock().Add(p.cfg.LeaseTTL), true)
		if rerr == nil && released != nil && released.Timeouts >= p.cfg.TimeoutBudget {
			// 超时熔断（v3 §1.4 对抗审查修正）：累计超时达阈值 → 杀实例重建，
			// 堵住「超时不杀」打开的僵尸负载通道（有 bug 的函数留下永不决议的
			// async 操作慢性塞满事件循环，health 仍响应、实例永不回收）。误
			// 熔断一次也只是 drain 语义重建，无害。
			TimeoutFuseTotal.WithLabelValues(req.ProjectID, req.FunctionID).Inc()
			p.killInstance(noCancel, ref, &rec)
		}
		return nil, status.Error(codes.DeadlineExceeded, "execution timed out")
	}
	if invokeErr != nil {
		// 传输层错误（连接拒绝/reset 等，非超时）＝容器崩溃判定（v3 §1.4
		// 表格，Cloud Run 同款）：仍杀整个实例（实例级隔离粒度，设计 §6
		// 生命周期与故障语义）。清账用独立 ctx（执行 ctx 可能已死）。
		p.killInstance(context.WithoutCancel(ctx), ref, &rec)
		return nil, status.Errorf(codes.Unavailable, "resident instance failed: %v", invokeErr)
	}

	// 正常完成（含函数报错封套——实例本身健康，保留复用）：释放 Lua 原子
	// 收账（inflight-1 + requests+1 + 续租；inflight==0 落 idle_since_ms 并
	// 按记录固化 max_requests 判 draining，v3 §1.3——runner 自退出后由
	// reaper 幽灵对账负责容器清理与计数回退）。
	_, _ = p.registry.Release(ctx, ref, rec.InstanceID, p.clock(), p.clock().Add(p.cfg.LeaseTTL), false)
	if res.Ok {
		resp := &ExecuteResponse{
			Status:     "ok",
			StdoutTail: truncateTail(res.Stdout),
			StderrTail: truncateTail(res.Stderr),
			DurationMS: duration.Milliseconds(),
		}
		if res.FnStatus > 0 {
			// fetch 风格（v3 §2.2 invoke 语义映射）：StatusCode 承载函数
			// HTTP status（v1/v2 为退出码语义位、恒 0）；body 双通道——
			// Response 为解码后文本（best-effort），ResponseB64 无损（64KB
			// 截断已在 runner 完成）；headers 随响应透传（仅触发器 sync
			// 路径消费，OQ7）。FnStatus 恒 ≥200：fetch 封套的 status 由
			// Response 构造器保证（200–599），main 风格封套无此字段。
			resp.StatusCode = res.FnStatus
			resp.Response = decodeB64BestEffort(res.BodyB64)
			resp.ResponseB64 = res.BodyB64
			resp.HTTPHeaders = res.Headers
		} else {
			resp.Response = res.Result
		}
		return resp, nil
	}
	// 函数报错（runner 500 封套）：实例本身健康，保留复用。
	return &ExecuteResponse{
		Status:     "error",
		StdoutTail: truncateTail(res.Stdout),
		StderrTail: truncateTail(res.Stderr),
		DurationMS: duration.Milliseconds(),
		Error:      firstNonEmpty(res.Error, "function failed"),
	}, nil
}

// killInstance 强杀实例并彻底清账（ContainerStop SIGKILL，复用 v1 原语；
// 幂等：容器已消失不报错）。
func (p *PoolManager) killInstance(ctx context.Context, ref FunctionRef, rec *InstanceRecord) {
	p.terminate(ctx, rec.ContainerID)
	_ = p.registry.Delete(ctx, ref, rec.InstanceID)
}

// terminate 停止并删除容器 + 回退常驻计数（幂等）。
func (p *PoolManager) terminate(ctx context.Context, containerID string) {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerCleanupTimeout)
	defer cancel()
	_ = p.daemon.StopInstance(tctx, containerID, 0)
	_ = p.daemon.RemoveInstance(tctx, containerID)
	p.mu.Lock()
	if p.counted[containerID] {
		delete(p.counted, containerID)
		p.residentTotal--
	}
	p.mu.Unlock()
}

// DrainForDeployment 在部署更新后排空旧 deployment 池（上限 ≤ 函数超时，
// 强杀后在途请求以错误收场——幂等键的适用场景，设计 §6）。
func (p *PoolManager) DrainForDeployment(ctx context.Context, projectID, functionID, keepDeploymentID string, grace time.Duration) {
	ref := FunctionRef{ProjectID: projectID, FunctionID: functionID}
	if grace <= 0 || grace > 300*time.Second {
		grace = 300 * time.Second
	}
	records, err := p.registry.List(ctx, ref)
	if err != nil {
		return
	}
	for i := range records {
		rec := records[i]
		if rec.DeploymentID == keepDeploymentID || rec.Draining {
			continue
		}
		_, _ = p.registry.Update(ctx, ref, rec.InstanceID, func(r *InstanceRecord) {
			r.Draining = true
		})
		if rec.Inflight > 0 {
			// 在途：宽限到点强杀（v3 §1.4：busy 布尔判定 → inflight>0；
			// 旧池 drain 上限 ≤ 函数超时；在途请求的分发连接被切断后由
			// executeOn 错误路径收场）。
			busy := rec
			// 宽限杀必须脱离请求 ctx 存活（请求方早已返回）——G118 误报。
			go func() { // #nosec G118 -- 宽限到点强杀须脱离请求 ctx 存活
				p.sleep(context.Background(), grace)
				p.killInstance(context.Background(), ref, &busy)
			}()
			continue
		}
		p.killInstance(ctx, ref, &rec)
	}
}

// Reaper 单轮对账：idle 回收 / 幽灵清理 / stuck-busy 强杀 / 水位与存活计量。
// 由 Service 周期调度（ReaperInterval）。
func (p *PoolManager) Reaper(ctx context.Context) {
	refs, err := p.registry.ListFunctions(ctx)
	if err != nil {
		return
	}
	now := p.clock()
	for _, ref := range refs {
		records, err := p.registry.List(ctx, ref)
		if err != nil {
			continue
		}
		// 第一遍：幽灵清理 + 分类。
		var idleCandidates []InstanceRecord
		alive := 0
		draining := 0
		inflightTotal := 0
		var uptimeMS float64
		for i := range records {
			rec := records[i]
			inflightTotal += rec.Inflight
			running, _, err := p.daemon.InspectInstance(ctx, rec.ContainerID)
			if err != nil || !running {
				// 幽灵/已退出实例（runner 达 max_requests 自退出也在此收敛）：
				// docker inspect 与注册表 diff 清理。
				_ = p.registry.Delete(ctx, ref, rec.InstanceID)
				p.revokeCounted(rec.ContainerID)
				continue
			}
			switch {
			case rec.Draining:
				draining++
				// runner 应自退出；仍存活超 30s 兜底强杀。
				if now.Sub(time.UnixMilli(rec.IdleSinceMS)) > 30*time.Second {
					p.killInstance(ctx, ref, &rec)
					draining--
				}
			case rec.Inflight > 0:
				// 判活规则：在途实例不因租约过期被回收（dispatch 认领 + 每次
				// 释放续租，v3 §1.4）；仅当租约过期超过 stuckBusyGrace（请求方
				// 已消失，如 dispatcher 重启）才强杀。
				if now.UnixMilli() > rec.LeaseUntilMS+stuckBusyGrace.Milliseconds() {
					p.killInstance(ctx, ref, &rec)
					continue
				}
				alive++
				uptimeMS += float64(p.cfg.ReaperInterval.Milliseconds())
			default:
				alive++
				uptimeMS += float64(p.cfg.ReaperInterval.Milliseconds())
				idleCandidates = append(idleCandidates, rec)
			}
		}
		// 第二遍：idle 回收——idle 超 TTL 且实例数 > min_instances（保温
		// 保底，策略随实例记录落账）才回收；从最旧 idle 开始。
		for _, rec := range idleCandidates {
			if alive <= rec.MinInstances {
				break
			}
			idleTTL := time.Duration(rec.IdleTTLSeconds) * time.Second
			if idleTTL <= 0 {
				idleTTL = p.cfg.IdleTTLDefault
			}
			if now.Sub(time.UnixMilli(rec.IdleSinceMS)) <= idleTTL {
				continue
			}
			p.killInstance(ctx, ref, &rec)
			alive--
		}
		// ——水位与存活计量（function_resident_uptime_ms：按实例存活累计，
		// min_instances>0 的保温成本显式计费，设计 §6 资源与计费）——
		p.mu.Lock()
		booting := p.booting[waiterKey(ref)]
		p.mu.Unlock()
		PoolReady.WithLabelValues(ref.ProjectID, ref.FunctionID).Set(float64(alive))
		PoolBooting.WithLabelValues(ref.ProjectID, ref.FunctionID).Set(float64(booting))
		PoolDraining.WithLabelValues(ref.ProjectID, ref.FunctionID).Set(float64(draining))
		ResidentUptimeMS.WithLabelValues(ref.ProjectID, ref.FunctionID).Add(uptimeMS)
		// 在途水位（v3 Observability：reaper 周期从注册表聚合 inflight 总和，
		// 与 PoolReady 同路）。
		InstanceInflight.WithLabelValues(ref.ProjectID, ref.FunctionID).Set(float64(inflightTotal))
	}
}

// ResidentTotal 返回当前进程内常驻实例数（观测/测试用）。
func (p *PoolManager) ResidentTotal() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.residentTotal
}

// revokeCounted 回退常驻计数（reaper 幽灵清理路径）。
func (p *PoolManager) revokeCounted(containerID string) {
	p.mu.Lock()
	if p.counted[containerID] {
		delete(p.counted, containerID)
		p.residentTotal--
	}
	p.mu.Unlock()
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// decodeB64BestEffort 解码 base64 为文本（失败原样返回——响应封套的
// body_base64 由 runner 生成，失败形态仅防御异常 runner）。
func decodeB64BestEffort(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s
	}
	return string(b)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncateTail(s string) string {
	if len(s) <= maxLogTailBytes {
		return s
	}
	return s[len(s)-maxLogTailBytes:]
}
