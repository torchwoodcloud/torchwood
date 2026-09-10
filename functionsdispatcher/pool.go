package functionsdispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

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
}

// runnerClient 是 runner 容器内 HTTP 协议的抽象。
type runnerClient interface {
	// Health 探针：实例就绪（ready）返回 nil。
	Health(ctx context.Context, ip string) error
	// Invoke 分发一次执行：body = TW_DATA JSON、header 带执行 token；
	// 超时/连接失败返回 error（调用方据此杀实例）。
	Invoke(ctx context.Context, ip string, data, token string, timeout time.Duration) (*invokeResult, error)
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

func (h *httpRunner) Invoke(ctx context.Context, ip string, data, token string, timeout time.Duration) (*invokeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, runnerURL(ip, "/"), bytes.NewReader([]byte(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		// 执行身份注入通道（P0.5 切换）：token 经分发 header 传递，runner
		// 侧逐请求写入 process.env（一期串行执行保证覆盖安全，见 runner 注释）。
		req.Header.Set("X-Tw-Execution-Token", token)
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLogTailBytes*2+4096))
	if err != nil {
		return nil, fmt.Errorf("read runner response: %w", err)
	}
	var envelope struct {
		Ok     bool   `json:"ok"`
		Result string `json:"result"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	return &invokeResult{
		HTTPStatus: resp.StatusCode,
		Ok:         envelope.Ok,
		Result:     envelope.Result,
		Stdout:     envelope.Stdout,
		Stderr:     envelope.Stderr,
		Error:      envelope.Error,
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
	return &PoolManager{
		daemon:   daemon,
		registry: registry,
		runner:   runner,
		cfg:      cfg,
		clock:    time.Now,
		sleep:    defaultSleep,
		waiters:  map[string]int{},
		booting:  map[string]int{},
		counted:  map[string]bool{},
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
	return PoolPolicy{
		MinInstances:           minInstances,
		MaxInstances:           maxInstances,
		IdleTTLSeconds:         idleTTL,
		MaxRequestsPerInstance: maxReq,
	}
}

// Dispatch 分发一次执行（热路径）：认领空闲实例 → runner HTTP；无空闲则
// 冷启动（池 0→1 扩容，spawn 收敛）或有界排队；超限 ResourceExhausted。
func (p *PoolManager) Dispatch(ctx context.Context, req ExecuteRequest) (*ExecuteResponse, error) {
	if req.ProjectID == "" || req.FunctionID == "" || req.DeploymentID == "" || req.Image == "" {
		return nil, status.Error(codes.InvalidArgument, "project/function/deployment/image are required")
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
			return p.executeOn(ctx, req, policy, *rec, started)
		}

		if err := p.trySpawn(ctx, req, policy); err != nil {
			// spawn 失败（daemon 故障等）：不立刻失败整个请求——继续等待
			// 其他请求的 spawn 成果或空闲实例，直至队首超时。ctx 取消例外。
			if errors.Is(err, context.Canceled) || status.Code(err) == codes.ResourceExhausted {
				return nil, err
			}
		}

		if !p.clock().Before(deadline) {
			DispatchQueueTimeouts.WithLabelValues(req.ProjectID, req.FunctionID).Inc()
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
		InstanceID:     inst.ContainerID,
		ContainerID:    inst.ContainerID,
		IP:             inst.IP,
		DeploymentID:   req.DeploymentID,
		Busy:           false,
		SpawnedAt:      now,
		IdleSince:      now,
		LeaseUntilMS:   now.Add(p.cfg.LeaseTTL).UnixMilli(),
		MinInstances:   policy.MinInstances,
		IdleTTLSeconds: policy.IdleTTLSeconds,
		MaxRequests:    policy.MaxRequestsPerInstance,
	}, nil
}

// executeOn 在已认领实例上分发请求并负责释放/回收。
func (p *PoolManager) executeOn(ctx context.Context, req ExecuteRequest, policy PoolPolicy, rec InstanceRecord, queuedAt time.Time) (*ExecuteResponse, error) {
	ref := FunctionRef{ProjectID: req.ProjectID, FunctionID: req.FunctionID}
	QueueWaitSeconds.WithLabelValues(req.ProjectID, req.FunctionID).Observe(p.clock().Sub(queuedAt).Seconds())
	started := p.clock()
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	res, invokeErr := p.runner.Invoke(ctx, rec.IP, req.Data, req.ExecutionToken, timeout)
	duration := p.clock().Sub(started)
	DispatchDurationSeconds.WithLabelValues(req.ProjectID, req.FunctionID).Observe(duration.Seconds())

	if invokeErr != nil {
		// 请求超时/调用方取消/容器崩溃 → 杀整个实例（隔离粒度 = 实例级，
		// 设计 §6 生命周期与故障语义）。清账用独立 ctx（执行 ctx 可能已死）。
		p.killInstance(context.WithoutCancel(ctx), ref, &rec)
		if ctx.Err() != nil || isTimeoutErr(invokeErr) {
			return nil, status.Error(codes.DeadlineExceeded, "execution timed out")
		}
		return nil, status.Errorf(codes.Unavailable, "resident instance failed: %v", invokeErr)
	}

	// 释放：busy=false + 请求数 + 续租；达 max_requests 标记 draining
	// （runner 响应后自退出，reaper 幽灵对账负责容器清理与计数回退）。
	_, _ = p.registry.Update(ctx, ref, rec.InstanceID, func(r *InstanceRecord) {
		now := p.clock()
		r.Busy = false
		r.IdleSince = now
		r.LeaseUntilMS = now.Add(p.cfg.LeaseTTL).UnixMilli()
		r.Requests++
		if policy.MaxRequestsPerInstance > 0 && r.Requests >= int64(policy.MaxRequestsPerInstance) {
			r.Draining = true
		}
	})
	if res.Ok {
		return &ExecuteResponse{
			Status:     "ok",
			Response:   res.Result,
			StdoutTail: truncateTail(res.Stdout),
			StderrTail: truncateTail(res.Stderr),
			DurationMS: duration.Milliseconds(),
		}, nil
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
		if rec.Busy {
			// 在途：宽限到点强杀（旧池 drain 上限 ≤ 函数超时；在途请求的
			// 分发连接被切断后由 executeOn 错误路径收场）。
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
		var uptimeMS float64
		for i := range records {
			rec := records[i]
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
				if now.Sub(rec.IdleSince) > 30*time.Second {
					p.killInstance(ctx, ref, &rec)
					draining--
				}
			case rec.Busy:
				// 判活规则：busy 实例不因心跳缺失被回收（dispatch 续租 +
				// busy 标记）；仅当租约过期超过 stuckBusyGrace（请求方已
				// 消失，如 dispatcher 重启）才强杀。
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
			if now.Sub(rec.IdleSince) <= idleTTL {
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
