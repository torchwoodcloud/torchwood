package dispatcher

// 本地 dind fleetly 端到端（IMPL-T2-3 守卫①的本地等价集成；编排脚本
// dispatcher/testdata/e2e/fleetly-dind.sh）：
//
//	TestE2EFleetlyTaskLifecycle —— 编排器（以「任务」形态跑在任务网络内的
//	  容器里）：miniredis 注册表 + 真实 fleetly 客户端（Tasks/build 面）+
//	  真实池 → EnsureProjectNetwork（幂等）→ Dispatch 冷启动 →
//	  fleetly CreateTask（spawn）→ 健康握手 → 请求分发 → TW_MAX_REQUESTS
//	  自退 → 平台回收（任务 stopped/服务移除）→ reaper 幽灵清理。
//
//	TestE2ERunnerServer —— 函数侧 runner 双（同一镜像以 -test.run 拉起）：
//	  /_tw/health + POST / 分发契约封套 + TW_MAX_REQUESTS 达阈自退（os.Exit，
//	  模拟真实 runner 的自回收语义）。
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
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/fleetly/server/v1"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestE2ERunnerServer 是函数侧 runner 契约双：与真实 runner 同形的最小
// HTTP 面（health + 分发封套），并按 TW_MAX_REQUESTS 自退。
func TestE2ERunnerServer(t *testing.T) {
	if os.Getenv("TW_E2E_RUNNER") != "1" {
		t.Skip("e2e runner double: set TW_E2E_RUNNER=1 (launched by the platform as the function task)")
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
			// 落盘/回包时间，然后进程退出 → 容器退出 → 平台任务 complete。
			go func() {
				time.Sleep(100 * time.Millisecond)
				os.Exit(0)
			}()
		}
	})
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", ":18080") //nolint:gosec // G102：e2e runner 双必须绑定容器全部网卡（平台经任务 DNS 从网络内探测/分发）
	require.NoError(t, err)
	t.Logf("e2e runner listening on :18080 (max_requests=%d)", maxRequests)
	_ = (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
}

// TestE2EFleetlyTaskLifecycle 是编排器（在任务网络内的任务容器里运行）：
// 完整跑通 dispatcher 的真实平台路径。
func TestE2EFleetlyTaskLifecycle(t *testing.T) {
	endpoint := os.Getenv("TW_E2E_FLEETLY_ENDPOINT")
	if endpoint == "" {
		t.Skip("dind fleetly e2e: set TW_E2E_FLEETLY_ENDPOINT (see dispatcher/testdata/e2e/fleetly-dind.sh)")
	}
	token := os.Getenv("TW_E2E_FLEETLY_TOKEN")
	imageRef := os.Getenv("TW_E2E_FUNCTION_IMAGE")
	require.NotEmpty(t, token, "TW_E2E_FLEETLY_TOKEN")
	require.NotEmpty(t, imageRef, "TW_E2E_FUNCTION_IMAGE")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 1) 真实 fleetly 客户端（Tasks/build 面）+ 内存注册表（miniredis 真 Lua）。
	cli, err := newGRPCFleetlyClient(endpoint, token)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	registry := NewRedisRegistry(rdb)

	cfg := &config.AppConfig{Functions: &config.Functions{
		Docker:     &config.Functions_Docker{},
		Dispatcher: &config.Functions_Dispatcher{BootTimeout: "90s"},
		Fleetly:    &config.Functions_Fleetly{Endpoint: endpoint, Token: token},
	}}
	daemon := newFleetlyDaemon(cfg, registry, cli, nil)
	daemon.pollInterval = 500 * time.Millisecond

	poolCfg := DefaultPoolConfig()
	poolCfg.BootTimeout = 90 * time.Second
	poolCfg.QueueHeadTimeout = 60 * time.Second
	poolCfg.PollInterval = 50 * time.Millisecond
	poolCfg.ReaperInterval = time.Second
	pool := NewPoolManager(daemon, registry, poolCfg)

	// 2) 任务网络 ensure（幂等；编排脚本已用同一 ref 建网——pe2e）。
	network, err := daemon.EnsureProjectNetwork(ctx, "e2e", false)
	require.NoError(t, err)
	require.Equal(t, "fleetly-taskgroup-pe2e", network)

	// 3) 逻辑镜像名 → 平台产物引用映射（编排脚本预构建的 runner 镜像）。
	logical := infrafunctions.ImageName(cfg, "fn1", "dep1")
	require.NoError(t, registry.SaveImageRef(ctx, logical, imageRef))

	// 4) 冷启动 → 健康握手 → 请求分发（两次：TW_MAX_REQUESTS=2 自退阈值）。
	req := ExecuteRequest{
		Image: logical, ProjectID: "e2e", FunctionID: "fn1", DeploymentID: "dep1",
		Runtime: "node-18.0", Spec: "shared-1x", TimeoutSeconds: 30,
		Data: `{"n":1}`,
		// runner 双自举开关（函数侧测试进程读取）。
		Env: map[string]string{"TW_E2E_RUNNER": "1"},
		Pool: PoolPolicy{
			MaxInstances:           1,
			MaxRequestsPerInstance: 2,
		},
	}
	resp1, err := pool.Dispatch(ctx, req)
	require.NoError(t, err, "首次分发（冷启动 + health 握手）")
	require.Equal(t, "ok", resp1.Status)
	require.Contains(t, resp1.Response, `"n":1`)

	records, err := registry.List(ctx, FunctionRef{ProjectID: "e2e", FunctionID: "fn1"})
	require.NoError(t, err)
	require.Len(t, records, 1, "冷启动后注册表必须有一条实例记录")
	taskID := records[0].ContainerID
	t.Logf("spawned task: %s (dns=%s)", taskID, records[0].IP)

	resp2, err := pool.Dispatch(ctx, req)
	require.NoError(t, err, "第二次分发（复用常驻实例）")
	require.Equal(t, "ok", resp2.Status)
	require.Contains(t, resp2.Response, `"n":2`)

	// 5) TW_MAX_REQUESTS 自退 → 平台回收（任务 complete → stopped，服务移除）。
	waitE2E(t, 3*time.Minute, func() bool {
		running, _, ierr := daemon.InspectInstance(ctx, taskID)
		return ierr == nil && !running
	}, "runner self-retire: task must leave running state")
	waitE2E(t, 3*time.Minute, func() bool {
		resp, gerr := cli.GetTask(ctx, &serverv1.GetTaskRequest{Id: taskID})
		return gerr == nil && resp.GetTask().GetStatus() == "stopped"
	}, "platform reclaim: task must reach stopped")
	respView, err := cli.GetTask(ctx, &serverv1.GetTaskRequest{Id: taskID})
	require.NoError(t, err)
	require.Equal(t, "exited", respView.GetTask().GetStopReason(),
		"restart-condition none 的自然退出语义")
	t.Logf("platform reclaimed task %s (stopped/exited)", taskID)

	// 6) reaper 幽灵清理：记录删除、常驻计数归零。
	pool.Reaper(ctx)
	records, err = registry.List(ctx, FunctionRef{ProjectID: "e2e", FunctionID: "fn1"})
	require.NoError(t, err)
	require.Empty(t, records, "reaper 必须清理自退实例的注册表记录")
	require.Zero(t, pool.ResidentTotal())

	// 7) 显式回收路径：Stop/Delete 幂等（台账行删除后 GetTask 404——引擎
	// 下一拍执行删除，轮询等待）。
	require.NoError(t, daemon.StopInstance(ctx, taskID, 0))
	require.NoError(t, daemon.RemoveInstance(ctx, taskID))
	waitE2E(t, time.Minute, func() bool {
		_, gerr := cli.GetTask(ctx, &serverv1.GetTaskRequest{Id: taskID})
		return status.Code(gerr) == codes.NotFound
	}, "DeleteTask 后台账行必须消失")
	require.NoError(t, daemon.RemoveInstance(ctx, taskID), "删除幂等：不存在视为成功")
	t.Log("E2E PASS: spawn -> health -> dispatch -> TW_MAX_REQUESTS self-retire -> platform reclaim")
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
