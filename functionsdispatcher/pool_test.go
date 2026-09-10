package functionsdispatcher

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---- fakes（池逻辑表驱动测试：fake daemon + fake registry + fake runner）----

type fakeDaemon struct {
	mu            sync.Mutex
	spawnCount    int
	spawned       map[string]string // containerID -> ip
	running       map[string]bool   // containerID -> running
	stopped       []string
	removed       []string
	builtImages   []string
	removedImages []string
	nextIP        int
	// networkFlags 记录 EnsureProjectNetwork 的 (project -> 最近一次 untrusted
	// 标志)（P2 egress 分类断言用）。
	networkFlags map[string]bool
	// lastNetwork 记录最近一次 SpawnInstance 收到的网络名。
	lastNetwork string
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		spawned:      map[string]string{},
		running:      map[string]bool{},
		nextIP:       1,
		networkFlags: map[string]bool{},
	}
}

func (d *fakeDaemon) EnsureProjectNetwork(_ context.Context, projectID string, untrusted bool) (string, error) {
	d.mu.Lock()
	d.networkFlags[projectID] = untrusted
	d.mu.Unlock()
	if untrusted {
		return "tw-func-" + projectID + "-int", nil
	}
	return "tw-func-" + projectID, nil
}

func (d *fakeDaemon) SpawnInstance(_ context.Context, opts SpawnOptions) (Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.spawnCount++
	d.lastNetwork = opts.Network
	id := fmt.Sprintf("cid-%d", d.spawnCount)
	ip := fmt.Sprintf("10.0.0.%d", d.nextIP)
	d.nextIP++
	d.spawned[id] = ip
	d.running[id] = true
	return Instance{ContainerID: id, IP: ip}, nil
}

func (d *fakeDaemon) InspectInstance(_ context.Context, containerID string) (bool, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.spawned[containerID]; !ok {
		return false, "", fmt.Errorf("no such container: %w", errdefs.ErrNotFound)
	}
	return d.running[containerID], d.spawned[containerID], nil
}

func (d *fakeDaemon) StopInstance(_ context.Context, containerID string, _ time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = append(d.stopped, containerID)
	d.running[containerID] = false
	return nil
}

func (d *fakeDaemon) RemoveInstance(_ context.Context, containerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, containerID)
	delete(d.spawned, containerID)
	return nil
}

func (d *fakeDaemon) BuildImage(_ context.Context, functionID, deploymentID string, _ []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.builtImages = append(d.builtImages, functionID+"-"+deploymentID)
	return nil
}

func (d *fakeDaemon) RemoveImage(_ context.Context, functionID, deploymentID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removedImages = append(d.removedImages, functionID+"-"+deploymentID)
	return nil
}

type fakeRegistry struct {
	mu    sync.Mutex
	pools map[string]map[string]*InstanceRecord
	locks map[string]bool
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{pools: map[string]map[string]*InstanceRecord{}, locks: map[string]bool{}}
}

func regKey(ref FunctionRef) string { return ref.ProjectID + ":" + ref.FunctionID }

func (r *fakeRegistry) List(_ context.Context, ref FunctionRef) ([]InstanceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []InstanceRecord
	for _, rec := range r.pools[regKey(ref)] {
		out = append(out, *rec)
	}
	return out, nil
}

func (r *fakeRegistry) ClaimIdle(_ context.Context, ref FunctionRef, deploymentID string, leaseUntil time.Time) (*InstanceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.pools[regKey(ref)] {
		if !rec.Busy && !rec.Draining && rec.DeploymentID == deploymentID {
			rec.Busy = true
			rec.LeaseUntilMS = leaseUntil.UnixMilli()
			out := *rec
			return &out, nil
		}
	}
	return nil, nil
}

func (r *fakeRegistry) Save(_ context.Context, ref FunctionRef, rec InstanceRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pool := r.pools[regKey(ref)]
	if pool == nil {
		pool = map[string]*InstanceRecord{}
		r.pools[regKey(ref)] = pool
	}
	stored := rec
	pool[rec.InstanceID] = &stored
	return nil
}

func (r *fakeRegistry) Update(_ context.Context, ref FunctionRef, instanceID string, mutate func(*InstanceRecord)) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pool := r.pools[regKey(ref)]
	if pool == nil || pool[instanceID] == nil {
		return false, nil
	}
	mutate(pool[instanceID])
	return true, nil
}

func (r *fakeRegistry) Delete(_ context.Context, ref FunctionRef, instanceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pool := r.pools[regKey(ref)]
	if pool != nil {
		delete(pool, instanceID)
	}
	return nil
}

func (r *fakeRegistry) ListFunctions(_ context.Context) ([]FunctionRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var refs []FunctionRef
	for k := range r.pools {
		parts := strings.SplitN(k, ":", 2)
		if len(parts) != 2 {
			continue
		}
		refs = append(refs, FunctionRef{ProjectID: parts[0], FunctionID: parts[1]})
	}
	return refs, nil
}

func (r *fakeRegistry) AcquireSpawnLock(_ context.Context, ref FunctionRef, _ time.Duration) (bool, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := "lock:" + regKey(ref)
	if r.locks[key] {
		return false, func() {}, nil
	}
	r.locks[key] = true
	return true, func() {
		r.mu.Lock()
		delete(r.locks, key)
		r.mu.Unlock()
	}, nil
}

type fakeRunner struct {
	mu           sync.Mutex
	healthy      bool
	invokeFn     func(ip string, data string) (*invokeResult, error)
	inFlight     int
	maxInFlight  int
	invokeCount  int
	invokedToken string
	invokedData  string
}

func (r *fakeRunner) Health(_ context.Context, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.healthy {
		return fmt.Errorf("not ready")
	}
	return nil
}

func (r *fakeRunner) Invoke(_ context.Context, ip string, data, token string, _ time.Duration) (*invokeResult, error) {
	r.mu.Lock()
	r.inFlight++
	r.invokeCount++
	if r.inFlight > r.maxInFlight {
		r.maxInFlight = r.inFlight
	}
	fn := r.invokeFn
	r.invokedToken = token
	r.invokedData = data
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}()
	if fn != nil {
		return fn(ip, data)
	}
	return &invokeResult{HTTPStatus: 200, Ok: true, Result: `{"ok":1}`}, nil
}

// newTestPool 组装被测池（真实时钟；表驱动用例按需覆盖参数）。
func newTestPool(d *fakeDaemon, reg *fakeRegistry, runner *fakeRunner, mutate func(*PoolConfig)) *PoolManager {
	cfg := PoolConfig{
		MaxResidentInstances: 8,
		QueueDepth:           32,
		QueueHeadTimeout:     2 * time.Second,
		BootTimeout:          2 * time.Second,
		ReaperInterval:       15 * time.Second,
		PollInterval:         time.Millisecond,
		LeaseTTL:             time.Minute,
		IdleTTLDefault:       300 * time.Second,
		MaxRequestsDefault:   1000,
		MaxInstancesDefault:  2,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return newPoolManager(d, reg, runner, cfg)
}

func dispatchReq() ExecuteRequest {
	return ExecuteRequest{
		Image:          "torchwood-funcs/func-fn1-dep1",
		ProjectID:      "p1",
		FunctionID:     "fn1",
		DeploymentID:   "dep1",
		Runtime:        "node-18.0",
		Spec:           "shared-1x",
		TimeoutSeconds: 5,
		Data:           `{"a":1}`,
		ExecutionToken: "twx_tok",
	}
}

// TestPoolDispatch_ColdStartThenReuse 冷启动 = 池 0→1 扩容；后续请求复用，
// 不再 spawn。
func TestPoolDispatch_ColdStartThenReuse(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()

	resp, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Equal(t, 1, d.spawnCount, "首请求冷启动 spawn")

	resp, err = pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Equal(t, 1, d.spawnCount, "第二请求复用常驻实例")

	// token 经分发 header 传递（P0.5 通道切换），data 走请求体。
	require.Equal(t, "twx_tok", runner.invokedToken)
	require.Equal(t, `{"a":1}`, runner.invokedData)
	require.Equal(t, 1, pool.ResidentTotal())
}

// TestPoolDispatch_SpawnConverged 同函数并发 spawn 收敛为一次：max_instances=1
// 时并发 8 请求只 spawn 1 个实例并全部串行复用（防 daemon 重启冷启动风暴）。
func TestPoolDispatch_SpawnConverged(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, func(c *PoolConfig) {
		c.MaxInstancesDefault = 1
		c.QueueDepth = 16
		c.QueueHeadTimeout = 10 * time.Second
	})
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = pool.Dispatch(ctx, dispatchReq())
		}(i)
	}
	wg.Wait()
	for i := range errs {
		require.NoError(t, errs[i])
	}
	require.Equal(t, 1, d.spawnCount, "并发请求必须收敛为单次 spawn")
	require.Equal(t, 1, runner.maxInFlight, "一期串行执行：单实例 1 并发")
}

// TestPoolDispatch_QueueDepthBounded 排队深度上限：满员后立即
// ResourceExhausted（有界排队，设计 §6）。
func TestPoolDispatch_QueueDepthBounded(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	block := make(chan struct{})
	runner.invokeFn = func(string, string) (*invokeResult, error) {
		<-block
		return &invokeResult{HTTPStatus: 200, Ok: true, Result: `{}`}, nil
	}
	pool := newTestPool(d, reg, runner, func(c *PoolConfig) {
		c.MaxInstancesDefault = 1
		c.QueueDepth = 2
		c.QueueHeadTimeout = 5 * time.Second
	})
	ctx := context.Background()

	// 1 个在途 + 2 个排队 = 满；第 4 个请求立即被拒。
	for i := 0; i < 3; i++ {
		go func() { _, _ = pool.Dispatch(ctx, dispatchReq()) }()
	}
	waitQueueDepth(t, pool, "p1:fn1", 2, 3*time.Second)

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "排队满必须立即拒绝")
	close(block)
}

func waitQueueDepth(t *testing.T, p *PoolManager, key string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := p.waiters[key]
		p.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("queue depth did not reach %d in time", want)
}

// TestPoolDispatch_QueueHeadTimeout 队首超时：无空闲且池满时在超时点返回
// ResourceExhausted，而非无界等在 ctx 上。
func TestPoolDispatch_QueueHeadTimeout(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	block := make(chan struct{})
	defer close(block)
	runner.invokeFn = func(string, string) (*invokeResult, error) {
		<-block
		return &invokeResult{HTTPStatus: 200, Ok: true, Result: `{}`}, nil
	}
	pool := newTestPool(d, reg, runner, func(c *PoolConfig) {
		c.MaxInstancesDefault = 1
		c.QueueHeadTimeout = 50 * time.Millisecond
	})
	ctx := context.Background()
	go func() { _, _ = pool.Dispatch(ctx, dispatchReq()) }()
	time.Sleep(30 * time.Millisecond)

	start := time.Now()
	_, err := pool.Dispatch(ctx, dispatchReq())
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Less(t, time.Since(start), 5*time.Second, "队首超时应远小于函数超时")
	// 池满路径没有任何 spawn 尝试：消息保持干净（last spawn error 仅在确有
	// 失败现场时拼接）。
	require.NotContains(t, status.Convert(err).Message(), "last spawn error")
}

// recordingHandler 捕获 slog 默认 logger 的消息（告警限频断言用）。
type recordingHandler struct {
	mu      sync.Mutex
	records []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Message)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.records...)
}

// TestPoolDispatch_QueueTimeoutCarriesLastSpawnError 被吞掉的 spawn 失败必须
// 留现场（EACCES 秒退事故的观测性修复）：spawn 成功但健康握手永不 ready
// （镜像坏档的等价形态）时，队首超时错误携带最后一次 spawn 失败原因，并以
// per function 限频 slog.Warn 落告警（重试循环按 PollInterval 反复 trySpawn，
// 不拦即刷屏）。
func TestPoolDispatch_QueueTimeoutCarriesLastSpawnError(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{} // healthy=false：健康握手永不 ready
	pool := newTestPool(d, reg, runner, func(c *PoolConfig) {
		c.BootTimeout = 30 * time.Millisecond
		c.QueueHeadTimeout = 250 * time.Millisecond
	})
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	records := &recordingHandler{}
	slog.SetDefault(slog.New(records))

	_, err := pool.Dispatch(context.Background(), dispatchReq())
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	msg := status.Convert(err).Message()
	require.Contains(t, msg, "no free instance within queue head timeout")
	require.Contains(t, msg, "last spawn error", "队首超时错误必须携带最后 spawn 失败原因")
	require.Contains(t, msg, "failed health probe", "真实根因（健康握手失败）必须出现在错误里")

	// 限频：测试窗口（~300ms）内反复重试只落一条告警（窗口 5s）。
	warns := 0
	for _, m := range records.snapshot() {
		if strings.Contains(m, "spawn attempt failed") {
			warns++
		}
	}
	require.Equal(t, 1, warns, "spawn 失败告警在限频窗口内只落一条: %v", records.snapshot())
}

// TestPoolDispatch_TimeoutKillsInstance 请求超时 → 杀整个实例（实例级隔离
// 粒度，设计 §6）并返回 DeadlineExceeded 语义。
func TestPoolDispatch_TimeoutKillsInstance(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	runner.invokeFn = func(string, string) (*invokeResult, error) {
		return nil, context.DeadlineExceeded
	}
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	require.Empty(t, reg.pools[regKey(FunctionRef{ProjectID: "p1", FunctionID: "fn1"})], "实例必须从注册表清除")
	require.Len(t, d.stopped, 1, "实例必须被强杀")
	require.Equal(t, 0, pool.ResidentTotal(), "常驻计数必须回退")
}

// TestPoolReaper_IdleReclaim 空闲回收：idle > TTL 且实例数 > min_instances。
func TestPoolReaper_IdleReclaim(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	require.Equal(t, 1, pool.ResidentTotal())

	// idle 超 TTL（记录 IdleTTL=300s）且 min_instances=0 → 回收。
	markIdleOld(t, reg, 400*time.Second, 0)
	pool.Reaper(ctx)

	require.Empty(t, reg.pools[regKey(FunctionRef{ProjectID: "p1", FunctionID: "fn1"})], "超时 idle 实例必须回收")
	require.Len(t, d.stopped, 1)
	require.Equal(t, 0, pool.ResidentTotal())
}

// TestPoolReaper_MinInstancesKeptWarm 保温：实例数 <= min_instances 不回收。
func TestPoolReaper_MinInstancesKeptWarm(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	// min_instances=1（保温）：即使 idle 超 TTL 也不回收。
	markIdleOld(t, reg, 400*time.Second, 1)
	pool.Reaper(ctx)

	records, err := reg.List(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"})
	require.NoError(t, err)
	require.Len(t, records, 1, "min_instances 保温不得回收")
	require.Empty(t, d.stopped)
}

// TestPoolReaper_GhostReconcile 幽灵对账：docker inspect 与注册表 diff——
// 容器已消失的注册表条目被清理。
func TestPoolReaper_GhostReconcile(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)

	// 容器被外部删除（daemon 重启等）→ inspect NotFound。
	d.mu.Lock()
	delete(d.spawned, "cid-1")
	d.mu.Unlock()
	pool.Reaper(ctx)

	require.Empty(t, reg.pools[regKey(FunctionRef{ProjectID: "p1", FunctionID: "fn1"})], "幽灵条目必须清理")
}

// TestPoolReaper_BusyNotKilledByLeaseExpiry 判活规则：busy 实例不因租约过期
// 被误杀；仅租约过期超过 stuckBusyGrace（请求方残留）才强杀。
func TestPoolReaper_BusyNotKilledByLeaseExpiry(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()

	rec, err := pool.spawnInstance(ctx, dispatchReq(), pool.applyDefaults(PoolPolicy{}))
	require.NoError(t, err)
	rec.Busy = true
	rec.LeaseUntilMS = time.Now().Add(time.Minute).UnixMilli()
	require.NoError(t, reg.Save(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"}, *rec))

	// 伪造「租约已过期但在宽限内」：直接改记录的租约为刚刚过期。
	expiredWithinGrace := time.Now().Add(-stuckBusyGrace / 2)
	_, _ = reg.Update(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"}, rec.InstanceID, func(r *InstanceRecord) {
		r.LeaseUntilMS = expiredWithinGrace.UnixMilli()
	})
	pool.Reaper(ctx)
	records, _ := reg.List(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"})
	require.Len(t, records, 1, "busy 实例不因心跳缺失被回收")

	// 租约过期超过 stuckBusyGrace：请求方已消失，强杀。
	expiredBeyondGrace := time.Now().Add(-2 * stuckBusyGrace)
	_, _ = reg.Update(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"}, rec.InstanceID, func(r *InstanceRecord) {
		r.LeaseUntilMS = expiredBeyondGrace.UnixMilli()
	})
	pool.Reaper(ctx)
	records, _ = reg.List(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"})
	require.Empty(t, records, "stuck-busy 残留实例必须强杀")
}

// TestPoolExecute_MaxRequestsDrains max_requests 到期实例响应后排空替换。
func TestPoolExecute_MaxRequestsDrains(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()
	req := dispatchReq()
	req.Pool = PoolPolicy{MaxRequestsPerInstance: 2}

	_, err := pool.Dispatch(ctx, req)
	require.NoError(t, err)
	_, err = pool.Dispatch(ctx, req)
	require.NoError(t, err)

	records, _ := reg.List(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"})
	require.Len(t, records, 1)
	require.True(t, records[0].Draining, "达 max_requests 后实例必须标记 draining")

	// runner 自退出后由 reaper 幽灵对账清理（容器 not running → 删记录）。
	d.mu.Lock()
	d.running["cid-1"] = false
	d.mu.Unlock()
	pool.Reaper(ctx)
	records, _ = reg.List(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"})
	require.Empty(t, records)
	require.Equal(t, 0, pool.ResidentTotal())
}

// TestDrainForDeployment 部署更新 = 旧 deployment 池 drain：空闲实例立即
// 回收，busy 实例宽限到点强杀。
func TestDrainForDeployment(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()

	now := time.Now()
	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	lease := now.Add(time.Minute).UnixMilli()
	require.NoError(t, reg.Save(ctx, ref, InstanceRecord{InstanceID: "old-idle", ContainerID: "old-idle", IP: "10.9.0.1",
		DeploymentID: "dep-old", SpawnedAt: now, IdleSince: now, LeaseUntilMS: lease}))
	require.NoError(t, reg.Save(ctx, ref, InstanceRecord{InstanceID: "old-busy", ContainerID: "old-busy", IP: "10.9.0.2",
		DeploymentID: "dep-old", Busy: true, SpawnedAt: now, IdleSince: now, LeaseUntilMS: lease}))
	require.NoError(t, reg.Save(ctx, ref, InstanceRecord{InstanceID: "new-1", ContainerID: "new-1", IP: "10.9.0.3",
		DeploymentID: "dep-new", SpawnedAt: now, IdleSince: now, LeaseUntilMS: lease}))
	d.spawned["old-idle"], d.spawned["old-busy"], d.spawned["new-1"] = "10.9.0.1", "10.9.0.2", "10.9.0.3"
	d.running["old-idle"], d.running["old-busy"], d.running["new-1"] = true, true, true

	pool.DrainForDeployment(ctx, "p1", "fn1", "dep-new", 20*time.Millisecond)

	ids := recordIDs(t, reg, ref)
	require.False(t, ids["old-idle"], "空闲旧实例立即回收")
	require.True(t, ids["old-busy"], "busy 旧实例宽限后回收")
	require.True(t, ids["new-1"], "新 deployment 实例保留")

	time.Sleep(60 * time.Millisecond) // 宽限到点强杀
	ids = recordIDs(t, reg, ref)
	require.False(t, ids["old-busy"], "busy 旧实例宽限到点强杀")
}

func recordIDs(t *testing.T, reg *fakeRegistry, ref FunctionRef) map[string]bool {
	t.Helper()
	records, err := reg.List(context.Background(), ref)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, r := range records {
		ids[r.InstanceID] = true
	}
	return ids
}

// TestApplyDefaults 池策略零值取平台默认（调用方零值时 dispatcher 侧兜底）。
func TestApplyDefaults(t *testing.T) {
	pool := newTestPool(newFakeDaemon(), newFakeRegistry(), &fakeRunner{}, nil)
	got := pool.applyDefaults(PoolPolicy{})
	require.Equal(t, 2, got.MaxInstances)
	require.Equal(t, 1000, got.MaxRequestsPerInstance)
	require.Equal(t, 300, got.IdleTTLSeconds)

	got = pool.applyDefaults(PoolPolicy{MaxInstances: 5, MinInstances: 9, IdleTTLSeconds: 10, MaxRequestsPerInstance: 3})
	require.Equal(t, 5, got.MaxInstances)
	require.Equal(t, 5, got.MinInstances, "min > max 时收敛到 max")
	require.Equal(t, 10, got.IdleTTLSeconds)
	require.Equal(t, 3, got.MaxRequestsPerInstance)
}

// TestDispatchValidation 缺参请求 InvalidArgument 语义。
func TestDispatchValidation(t *testing.T) {
	pool := newTestPool(newFakeDaemon(), newFakeRegistry(), &fakeRunner{}, nil)
	_, err := pool.Dispatch(context.Background(), ExecuteRequest{ProjectID: "p1"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// markIdleOld 把实例记录标记为「已空闲超过 ttl」（reaper 表驱动；min 为
// 该记录落账的 min_instances）。
func markIdleOld(t *testing.T, reg *fakeRegistry, idle time.Duration, minInstances int) {
	t.Helper()
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	ok, err := reg.Update(ctx, ref, "cid-1", func(r *InstanceRecord) {
		r.IdleSince = time.Now().Add(-idle)
		r.IdleTTLSeconds = 300
		r.MinInstances = minInstances
	})
	require.NoError(t, err)
	require.True(t, ok)
}

// TestPoolDispatch_EgressNetworkSelection（P2 egress 分类）：untrusted 请求
// 走 internal 变体网络（tw-func-<project>-int），trusted 请求走常规网络；
// dispatcher 对两类网络都按需 join（fake 断言标志与选网）。
func TestPoolDispatch_EgressNetworkSelection(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	ctx := context.Background()

	// 不可信函数（client_callable / 有触发器，分类在 app 层完成）。
	untrusted := dispatchReq()
	untrusted.EgressUntrusted = true
	resp, err := pool.Dispatch(ctx, untrusted)
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.True(t, d.networkFlags["p1"])
	require.Equal(t, "tw-func-p1-int", d.lastNetwork)

	// 可信函数（server key 触发）：常规网络。
	trusted := dispatchReq()
	trusted.FunctionID = "fn_trusted"
	resp, err = pool.Dispatch(ctx, trusted)
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.False(t, d.networkFlags["p1"])
	require.Equal(t, "tw-func-p1", d.lastNetwork)
}
