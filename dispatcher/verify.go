package dispatcher

// 本文件是双执行底座共享的部署后验证 spawn 面（设计 §1/D10）：构建完成后
// spawn 一枚一次性实例探活，失败携带任务现场。两种驱动经 SpawnVerifyInstance
// 消费同一编排；生产探针与清理超时同在此单点。

import (
	"context"
	"fmt"
	"time"
)

// CleanupTimeout 是清理类平台/daemon 操作（stop/delete/remove）的独立超时：
// 不继承已取消的执行 ctx，也不允许平台挂起时无限阻塞（两种驱动形态同约定，
// dockerdriver 经 dispatcher.CleanupTimeout 消费同值）。
const CleanupTimeout = 30 * time.Second

// 验证 spawn 的 health 探针节拍（对齐池启动握手 spawnInstance：单次探针
// 2s 超时、轮询间隔 100ms；预算本身 = 本进程 boot_timeout，构造时解析）。
const (
	verifyProbeTimeout = 2 * time.Second
	verifyPollInterval = 100 * time.Millisecond
)

// verifySpawnConfig 是验证 spawn 的参数包：镜像引用 + 探针预算 + 注入值。
// PollInterval 零值取 verifyPollInterval（单测注入加速/确定性）。
type verifySpawnConfig struct {
	Image        string
	BootTimeout  time.Duration
	MaxRequests  int
	PollInterval time.Duration
}

// HealthProber 是验证探针的最小抽象（runnerClient 的 Health 面收窄；生产 =
// httpRunner，单测 = fake 表驱动）。IMPL-T2-5 导出：dockerdriver 驱动的
// 验证 spawn 复用同一探针注入缝。
type HealthProber interface {
	Health(ctx context.Context, ip string) error
}

// NewHTTPProber 返回验证 spawn 的生产探针（/_tw/health HTTP 客户端，与池
// 的启动握手同款节拍）。
func NewHTTPProber() HealthProber { return newHTTPRunner() }

// SpawnVerifyInstance 执行一次部署后验证 spawn 的导出入口（扁平参数形态，
// dockerdriver 消费此签名）：ensure 网络 → spawn → /_tw/health 轮询（预算
// = bootTimeout）→ 就绪或失败现场回收；image 须为执行底座可直接解析的引用。
// 两种驱动的 BuildImage / ImportImage 共用同一编排；probe 是验证探针（生产
// = NewHTTPProber，测试可注入 fake）；maxRequests 是验证实例 TW_MAX_REQUESTS
// 注入值。
func SpawnVerifyInstance(ctx context.Context, d InstanceSupervisor, probe HealthProber, opts BuildImageOptions, image string, bootTimeout time.Duration, maxRequests int) error {
	return spawnVerifyInstance(ctx, d, probe, opts, verifySpawnConfig{
		Image:       image,
		BootTimeout: bootTimeout,
		MaxRequests: maxRequests,
	})
}

// spawnVerifyInstance 是验证编排的本体（verifySpawnConfig 形态，包内两条腿
// 复用：BuildImage 直接探活 / ImportImage 先读钉定引用再探活）。
func spawnVerifyInstance(ctx context.Context, d InstanceSupervisor, probe HealthProber, opts BuildImageOptions, vc verifySpawnConfig) error {
	// egress 分类与执行一致（对抗审查 A1 最强修复）：untrusted 函数的验证
	// 实例挂 internal 变体任务网络（与池 spawnInstance 同路）——验证期不得
	// 给不可信镜像开跳出网窗口。
	network, err := d.EnsureProjectNetwork(ctx, opts.ProjectID, opts.EgressUntrusted)
	if err != nil {
		return fmt.Errorf("verify spawn: ensure network: %w", err)
	}
	inst, err := spawnVerificationTask(ctx, d, opts, vc, network)
	if err != nil {
		return fmt.Errorf("verify spawn: %w", err)
	}
	// 无论成败回收任务（验证实例无在途请求，无需 drain 宽限）。
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CleanupTimeout)
		defer cancel()
		_ = d.StopInstance(cctx, inst.ContainerID)
		_ = d.RemoveInstance(cctx, inst.ContainerID)
	}()
	return awaitVerificationHealthy(ctx, d, probe, opts, vc, inst)
}

// spawnVerificationTask 创建验证任务（env 组装与池 spawnInstance 同源
// （SanitizeRunnerEnv：TW_DATA/TW_EXECUTION_TOKEN 不进容器 env——常驻的是
// 容器不是凭证；TW_MAX_REQUESTS/TW_DRAIN_TIMEOUT_MS 由驱动侧
// AppendRunnerControlEnv 统一追加）。
func spawnVerificationTask(ctx context.Context, d InstanceSupervisor, opts BuildImageOptions, vc verifySpawnConfig, network string) (Instance, error) {
	return d.SpawnInstance(ctx, SpawnOptions{
		ProjectID:   opts.ProjectID,
		FunctionID:  opts.FunctionID,
		Image:       vc.Image,
		Network:     network,
		Env:         SanitizeRunnerEnv(opts.Env),
		Spec:        "shared-1x",
		MaxRequests: vc.MaxRequests,
		Name:        containerName(opts.ProjectID, opts.FunctionID),
	})
}

// awaitVerificationHealthy 轮询验证实例的 /_tw/health 直到就绪（预算 =
// BootTimeout）；失败时把任务台账失败现场拼进错误。
func awaitVerificationHealthy(ctx context.Context, d InstanceSupervisor, probe HealthProber, opts BuildImageOptions, vc verifySpawnConfig, inst Instance) error {
	interval := vc.PollInterval
	if interval <= 0 {
		interval = verifyPollInterval
	}
	deadline := time.Now().Add(vc.BootTimeout)
	for {
		pctx, pcancel := context.WithTimeout(ctx, verifyProbeTimeout)
		err := probe.Health(pctx, inst.IP)
		pcancel()
		if err == nil {
			return nil // 验证通过：静默（成功不产生 deployment.error）
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			// 失败处置：台账读取与后续清理同用脱离构建 ctx 的独立 ctx
			//（预算到点时构建 ctx 可能已取消/临期）。
			tailCtx, tailCancel := context.WithTimeout(context.WithoutCancel(ctx), CleanupTimeout)
			tail, tailErr := d.InstanceLogsTail(tailCtx, inst.ContainerID, maxLogTailBytes)
			tailCancel()
			if tailErr != nil {
				tail = fmt.Sprintf("<task status unavailable: %v>", tailErr)
			}
			return fmt.Errorf("verification failed: function image did not become healthy within %s (last probe error: %v); task status:\n%s",
				vc.BootTimeout, err, tail)
		}
		if !defaultSleep(ctx, interval) {
			// sleep 期间 ctx 取消：下一轮探针立即失败并走上面的现场回收路径。
			continue
		}
	}
}
