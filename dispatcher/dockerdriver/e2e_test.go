package dockerdriver

// docker 直接执行底座的本地端到端（IMPL-T2-5 守卫②的本地等价集成；编排
// 脚本 dispatcher/testdata/e2e/docker-driver-e2e.sh）：
//
//	TestE2EDockerDriverLifecycle —— 编排器（容器内运行，挂载 docker.sock）：
//	  miniredis 注册表 + 真实 docker daemon 客户端（本包 New）+ 真实池 →
//	  EnsureProjectNetwork（创建 tw-func-e2e + 自 attach）→ Dispatch 冷启动 →
//	  SpawnInstance（容器 spawn + 加固）→ 健康握手 → 请求分发 →
//	  TW_MAX_REQUESTS 自退 → 容器退出 → reaper 幽灵清理 → Stop/Remove 幂等。
//
//	TestE2EDockerRunnerServer —— 函数侧 runner 双（同一镜像以缺省 ENTRYPOINT
//	  拉起）：/_tw/health + POST / 分发契约封套 + TW_MAX_REQUESTS 达阈自退
//	  （os.Exit，模拟真实 runner 的自回收语义）。
//
// 两者默认跳过（环境变量未设），不污染常规 go test。

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/dispatcher"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// TestE2EDockerRunnerServer 是函数侧 runner 契约双：与真实 runner 同形的最小
// HTTP 面（health + 分发封套），并按 TW_MAX_REQUESTS 自退。
func TestE2EDockerRunnerServer(t *testing.T) {
	if os.Getenv("TW_E2E_RUNNER") != "1" {
		t.Skip("e2e runner double: set TW_E2E_RUNNER=1 (launched by the platform as the function container)")
	}
	maxRequests, _ := strconv.Atoi(os.Getenv("TW_MAX_REQUESTS"))
	if maxRequests <= 0 {
		maxRequests = 1000
	}
	var served atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/_tw/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		n := served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"n":%d,"echo":%q}}`, n, string(body))
		t.Logf("e2e runner served request #%d (max_requests=%d)", n, maxRequests)
		if n >= int64(maxRequests) {
			// 自退（真实 runner 达 max_requests 后的自回收语义）：给响应一点
			// 落盘/回包时间，然后进程退出 → 容器退出。
			go func() {
				time.Sleep(100 * time.Millisecond)
				os.Exit(0)
			}()
		}
	})
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", ":18080") //nolint:gosec // G102：e2e runner 双必须绑定容器全部网卡（池经容器 IP 从网络内探测/分发）
	require.NoError(t, err)
	t.Logf("e2e runner listening on :18080 (max_requests=%d)", maxRequests)
	_ = (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
}

// TestE2EDockerDriverLifecycle 是编排器（容器内运行，挂载 docker.sock）：
// 完整跑通 dispatcher 的 docker 直接执行路径。
func TestE2EDockerDriverLifecycle(t *testing.T) {
	if os.Getenv("TW_E2E_DOCKER") != "1" {
		t.Skip("docker driver e2e: set TW_E2E_DOCKER=1 (see dispatcher/testdata/e2e/docker-driver-e2e.sh)")
	}
	imageRef := os.Getenv("TW_E2E_FUNCTION_IMAGE")
	require.NotEmpty(t, imageRef, "TW_E2E_FUNCTION_IMAGE")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// 1) 真实 docker daemon 客户端（默认 unix:///var/run/docker.sock，由
	//    编排器容器挂载）+ 内存注册表（miniredis 真 Lua）。
	cfg := &config.AppConfig{Functions: &config.Functions{
		Driver:     config.FunctionsDriverDocker,
		Docker:     &config.Functions_Docker{},
		Dispatcher: &config.Functions_Dispatcher{BootTimeout: "60s"},
	}}
	daemon, err := New(cfg)
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	registry := dispatcher.NewRedisRegistry(rdb)

	poolCfg := dispatcher.DefaultPoolConfig()
	poolCfg.BootTimeout = 60 * time.Second
	poolCfg.QueueHeadTimeout = 60 * time.Second
	poolCfg.PollInterval = 50 * time.Millisecond
	poolCfg.ReaperInterval = time.Second
	pool := dispatcher.NewPoolManager(daemon, registry, poolCfg)

	// 2) 项目网络 ensure（创建 tw-func-e2e + 自 attach 编排器容器；幂等）。
	network, err := daemon.EnsureProjectNetwork(ctx, "e2e", false)
	require.NoError(t, err)
	require.Equal(t, "tw-func-e2e", network)

	// 3) 冷启动 → 健康握手 → 请求分发（两次：TW_MAX_REQUESTS=2 自退阈值）。
	//    docker 形态逻辑名即本地 tag（零产物引用映射）。
	req := dispatcher.ExecuteRequest{
		Image: imageRef, ProjectID: "e2e", FunctionID: "fn1", DeploymentID: "dep1",
		Spec: "shared-1x", TimeoutSeconds: 30,
		Data: `{"n":1}`,
		// runner 双自举开关（函数侧测试进程读取）。
		Env: map[string]string{"TW_E2E_RUNNER": "1"},
		Pool: dispatcher.PoolPolicy{
			MaxInstances:           1,
			MaxRequestsPerInstance: 2,
		},
	}
	resp1, err := pool.Dispatch(ctx, req)
	require.NoError(t, err, "首次分发（冷启动 + health 握手）")
	require.Equal(t, "ok", resp1.Status)
	require.Contains(t, resp1.Response, `"n":1`)

	records, err := registry.List(ctx, dispatcher.FunctionRef{ProjectID: "e2e", FunctionID: "fn1"})
	require.NoError(t, err)
	require.Len(t, records, 1, "冷启动后注册表必须有一条实例记录")
	containerID := records[0].ContainerID
	t.Logf("spawned container: %s (ip=%s)", containerID, records[0].IP)

	resp2, err := pool.Dispatch(ctx, req)
	require.NoError(t, err, "第二次分发（复用常驻实例）")
	require.Equal(t, "ok", resp2.Status)
	require.Contains(t, resp2.Response, `"n":2`)

	// 4) TW_MAX_REQUESTS 自退 → 容器退出（running 离场）。
	waitE2E(t, 2*time.Minute, func() bool {
		running, _, ierr := daemon.InspectInstance(ctx, containerID)
		return ierr == nil && !running
	}, "runner self-retire: container must leave running state")
	t.Log("runner self-retired (container exited)")

	// 5) reaper 幽灵清理：记录删除、常驻计数归零。
	pool.Reaper(ctx)
	records, err = registry.List(ctx, dispatcher.FunctionRef{ProjectID: "e2e", FunctionID: "fn1"})
	require.NoError(t, err)
	require.Empty(t, records, "reaper 必须清理自退实例的注册表记录")
	require.Zero(t, pool.ResidentTotal())

	// 6) 显式回收路径：Stop/Remove 幂等（容器已退场，不存在视为成功）。
	require.NoError(t, daemon.StopInstance(ctx, containerID, 0))
	require.NoError(t, daemon.RemoveInstance(ctx, containerID))
	require.NoError(t, daemon.RemoveInstance(ctx, containerID), "删除幂等：不存在视为成功")
	t.Log("E2E PASS: spawn -> health -> dispatch -> TW_MAX_REQUESTS self-retire -> reaper reclaim -> idempotent Stop/Remove")
}

func waitE2E(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s: %s", timeout, msg)
}
