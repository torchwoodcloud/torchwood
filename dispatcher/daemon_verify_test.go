package dispatcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件覆盖部署后验证 spawn（Go 一期阶段 3，设计
// docs/design/functions-runtimes-and-sources.md §1「部署后验证 spawn」/D10）：
// fake daemon + fake health 表驱动——就绪成功/超时失败/日志尾拼接/egress
// 选网参数/cleanup 总是被调用，不依赖真实 daemon。

// fakeHealth 是 healthProber 的可编程 fake：前 fails 次 Health 返回错误
// （fails < 0 = 永远失败），之后成功。
type fakeHealth struct {
	mu       sync.Mutex
	fails    int
	err      error
	attempts int
}

func (h *fakeHealth) Health(_ context.Context, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attempts++
	if h.fails < 0 || h.attempts <= h.fails {
		if h.err != nil {
			return h.err
		}
		return errors.New("connection refused")
	}
	return nil
}

// logsErrDaemon 在 fakeDaemon 之上把 InstanceLogsTail 改为恒失败（日志回收
// 降级路径断言用）。
type logsErrDaemon struct{ *fakeDaemon }

func (d *logsErrDaemon) InstanceLogsTail(context.Context, string, int64) (string, error) {
	return "", errors.New("daemon unreachable")
}

func verifyOpts() BuildImageOptions {
	return BuildImageOptions{
		ProjectID:    "p1",
		FunctionID:   "fn1",
		DeploymentID: "dep1",
		Env:          map[string]string{"GREETING": "hi"},
	}
}

func verifyConfig() verifySpawnConfig {
	// 毫秒级预算 + 1ms 轮询：表驱动用例确定性收敛，无真实等待。
	return verifySpawnConfig{Image: "img-ref", BootTimeout: 50 * time.Millisecond, MaxRequests: 1000, PollInterval: time.Millisecond}
}

// TestSpawnVerifyInstance_Table 表驱动主路径：就绪成功（首探即就绪/重试后
// 就绪）与超时失败（日志尾拼接 / 日志不可用降级）；cleanup（Stop+Remove）
// 在成功与失败路径都必须被调用（池外实例用完即删）。
func TestSpawnVerifyInstance_Table(t *testing.T) {
	const panicLog = `tw_runner startup line
panic: protocol not implemented
goroutine 1 [running]:`

	cases := []struct {
		name        string
		fails       int // fakeHealth 前 N 次失败；<0 = 永远失败
		logs        string
		logsBroken  bool
		wantErr     string // 空 = 期望成功
		wantTailHas string // 期望错误信息包含的日志尾片段
	}{
		{
			name:    "ready-on-first-probe",
			fails:   0,
			wantErr: "",
		},
		{
			name:    "ready-after-retries",
			fails:   3,
			wantErr: "",
		},
		{
			name:        "timeout-appends-log-tail",
			fails:       -1,
			logs:        panicLog,
			wantErr:     "verification failed",
			wantTailHas: "panic: protocol not implemented",
		},
		{
			name:        "timeout-without-logs-still-fails",
			fails:       -1,
			wantErr:     "did not become healthy within 50ms",
			wantTailHas: "",
		},
		{
			name:        "log-collection-failure-degrades-gracefully",
			fails:       -1,
			logsBroken:  true,
			wantErr:     "<container logs unavailable: daemon unreachable>",
			wantTailHas: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon()
			var daemon Daemon = d
			if tc.logsBroken {
				daemon = &logsErrDaemon{fakeDaemon: d}
			}
			probe := &fakeHealth{fails: tc.fails}
			vc := verifyConfig()
			d.logs["cid-1"] = tc.logs

			err := spawnVerifyInstance(context.Background(), daemon, probe, verifyOpts(), vc)

			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				if tc.wantTailHas != "" {
					require.Contains(t, err.Error(), tc.wantTailHas,
						"错误信息必须携带容器日志尾部（panic/协议未实现的第一现场）")
				}
			}
			// cleanup 总是被调用：无论成败，验证容器 Stop(SIGKILL)+Remove。
			require.Equal(t, []string{"cid-1"}, d.stopped, "验证容器必须被停止")
			require.Equal(t, []string{"cid-1"}, d.removed, "验证容器必须被删除")
			// 预算为毫秒级：断言探针确实发生了多次轮询重试（非首探即弃）。
			probe.mu.Lock()
			attempts := probe.attempts
			probe.mu.Unlock()
			if tc.fails != 0 {
				require.Greater(t, attempts, 1, "探针必须按间隔轮询而非首探即弃")
			}
		})
	}
}

// TestSpawnVerifyInstance_EgressNetworkSelection egress 分类与执行一致
// （对抗审查 A1）：untrusted 函数的验证实例必须挂 internal 变体网络
// （与池 spawnInstance 同路），常规函数挂常规网络。
func TestSpawnVerifyInstance_EgressNetworkSelection(t *testing.T) {
	cases := []struct {
		name        string
		untrusted   bool
		wantNetwork string
	}{
		{"trusted-regular-network", false, "tw-func-p1"},
		{"untrusted-internal-network", true, "tw-func-p1-int"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon()
			opts := verifyOpts()
			opts.EgressUntrusted = tc.untrusted

			require.NoError(t, spawnVerifyInstance(context.Background(), d, &fakeHealth{}, opts, verifyConfig()))

			require.Equal(t, tc.untrusted, d.networkFlags["p1"], "EnsureProjectNetwork 必须收到 egress 分类标志")
			require.Equal(t, tc.wantNetwork, d.lastNetwork, "验证实例必须挂与执行同路的网络")
			require.Equal(t, tc.wantNetwork, d.lastSpawn.Network)
		})
	}
}

// TestSpawnVerifyInstance_EnvAssembly env 组装与池 spawnInstance 同源：
// 函数 variables 透传、TW_DATA/TW_EXECUTION_TOKEN 不进容器 env（常驻的是
// 容器不是凭证）、TW_MAX_REQUESTS 注入值对齐池缺省、Spec 取 shared-1x。
func TestSpawnVerifyInstance_EnvAssembly(t *testing.T) {
	d := newFakeDaemon()
	opts := verifyOpts()
	opts.Env = map[string]string{
		"GREETING":           "hi",
		"TW_DATA":            `{"secret":"must-not-leak"}`,
		"TW_EXECUTION_TOKEN": "twx_must-not-leak",
	}
	vc := verifyConfig()

	require.NoError(t, spawnVerifyInstance(context.Background(), d, &fakeHealth{}, opts, vc))

	env := d.lastSpawn.Env
	require.Contains(t, env, "GREETING=hi")
	require.NotContains(t, env, "TW_DATA="+opts.Env["TW_DATA"], "TW_DATA 不得进容器 env")
	require.NotContains(t, env, "TW_EXECUTION_TOKEN="+opts.Env["TW_EXECUTION_TOKEN"], "执行 token 不得进容器 env")
	require.Equal(t, 1000, d.lastSpawn.MaxRequests, "TW_MAX_REQUESTS 注入值对齐池 MaxRequestsDefault")
	require.Equal(t, "shared-1x", d.lastSpawn.Spec)
	require.Equal(t, "img-ref", d.lastSpawn.Image, "验证实例必须挂刚构建的镜像")
	require.Equal(t, "p1", d.lastSpawn.ProjectID)
}

// TestNewDockerDaemon_VerifyBudgetFromConfig boot 预算来源接线：daemon 从
// config 解析本进程 boot_timeout（= 池 PoolConfig.BootTimeout 同一 config
// 键同一解析规则）与 MaxRequests 缺省（1000），不引入构造参数。
func TestNewDockerDaemon_VerifyBudgetFromConfig(t *testing.T) {
	d := NewDockerDaemon(&config.AppConfig{Functions: &config.Functions{
		Docker:     &config.Functions_Docker{},
		Dispatcher: &config.Functions_Dispatcher{BootTimeout: "3s"},
	}})
	dd, ok := d.(*dockerDaemon)
	require.True(t, ok)
	require.Equal(t, 3*time.Second, dd.bootTimeout)
	require.Equal(t, 1000, dd.maxRequestsDefault)

	// 未配置 boot_timeout = 平台默认 60s（与池缺省一致）。
	d2 := NewDockerDaemon(&config.AppConfig{Functions: &config.Functions{
		Docker: &config.Functions_Docker{},
	}})
	dd2, ok := d2.(*dockerDaemon)
	require.True(t, ok)
	require.Equal(t, defaultBootTimeout, dd2.bootTimeout)
}
