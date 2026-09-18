package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖四期 4b M1 镜像全局化与 registry 路由模式的 fake 表驱动测试
// （设计 docs/design/functions-runtimes-and-sources.md §4 M1/M7）：
//   - push 门控（dispatcherPushEnabled）与 push 编排（pushBuiltImage：
//     失败 = 构建失败、RegistryAuth 留空 = daemon 侧凭证）；
//   - pull 编排（ensureImage：本地命中零 pull / miss 触发 pull / 失败含
//     pull 摘要）；
//   - 池级门控：registry 模式 spawn 前 EnsureImage、local 模式永不 pull；
//   - 路由语义：registry 模式冷启动不转发（handled=false 本地 spawn），
//     实例亲和转发保持不变。
// 真实 daemon 的 push/pull 端到端在 registry_integration_test.go（docker
// 门控）；路由 local 模式决策表回归在 routing_test.go。

// TestDispatcherPushEnabled_Gating push 门控（构建链尾部）：registry 模式且
// registry_push=true 才 push；local 模式忽略 registry_push（镜像不分发）；
// 未知 routing_mode（启动期校验拦截在先）按 local 兜底。
func TestDispatcherPushEnabled_Gating(t *testing.T) {
	cases := []struct {
		name         string
		routingMode  string
		registryPush bool
		want         bool
	}{
		{"registry+push → push", "registry", true, true},
		{"registry 未开 push → 不 push（启动期校验拒绝的组合，兜底）", "registry", false, false},
		{"local+push=true → 不 push（local 忽略该值）", "local", true, false},
		{"local 缺省 → 不 push", "", true, false},
		{"未知值 → 不 push（fail-safe 缺省）", "bogus", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.AppConfig{Functions: &config.Functions{
				Dispatcher: &config.Functions_Dispatcher{
					RoutingMode:  tc.routingMode,
					RegistryPush: tc.registryPush,
				},
			}}
			require.Equal(t, tc.want, dispatcherPushEnabled(cfg))
		})
	}
}

// TestPoolConfigFromConfig_RoutingMode 路由模式进池配置：config 原始值经
// NormalizedFunctionsRoutingMode 归一（空 → local），非法值原样保留
// （池按非 registry 处理 = local 语义 fail-safe；启动期校验拦截在先）。
func TestPoolConfigFromConfig_RoutingMode(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", config.FunctionsRoutingModeLocal},
		{"local", config.FunctionsRoutingModeLocal},
		{"registry", config.FunctionsRoutingModeRegistry},
		{"bogus", "bogus"}, // 校验拦截在先；池侧 fail-safe 按 local 处理
	}
	for _, tc := range cases {
		cfg := &config.AppConfig{Functions: &config.Functions{
			Dispatcher: &config.Functions_Dispatcher{RoutingMode: tc.raw},
		}}
		require.Equal(t, tc.want, PoolConfigFromConfig(cfg).RoutingMode, "raw=%q", tc.raw)
	}
}

// TestPushBuiltImage_Table push 编排（fakeImageClient 驱动）：成功零 RegistryAuth
// （daemon 侧已登录凭证）；传输失败 / 流内 JSON error（registry 认证失败等）
// 一律以含 push 摘要的明确错误收场（push 失败 = 构建失败）。
func TestPushBuiltImage_Table(t *testing.T) {
	const ref = "127.0.0.1:5500/func-fn1-dep1"

	cases := []struct {
		name    string
		errs    map[string][]error
		streams map[string]string
		wantErr string // 空 = 期望成功
	}{
		{
			name:    "push 成功",
			wantErr: "",
		},
		{
			name:    "push 传输失败携带摘要",
			errs:    map[string][]error{"push/" + ref: {errors.New("connection refused")}},
			wantErr: `docker push "` + ref + `" failed`,
		},
		{
			name: "push 流内错误（registry 认证失败）携带摘要",
			streams: map[string]string{
				"push/" + ref: `{"errorDetail":{"message":"unauthorized: authentication required"}}` + "\n",
			},
			wantErr: "unauthorized: authentication required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imgs := newFakeImageClient()
			imgs.errs = tc.errs
			for k, v := range tc.streams {
				imgs.pullStreams[k] = v
			}

			err := pushBuiltImage(context.Background(), imgs, ref)

			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			require.Equal(t, []string{"push " + ref}, imgs.calls, "必须恰好一次 push 调用")
			require.Equal(t, ref, imgs.lastPushRef)
			require.Empty(t, imgs.lastPushAuth, "RegistryAuth 必须留空 = daemon 侧已登录凭证")
		})
	}
}

// TestEnsureImage_Table pull 编排（fakeImageClient 驱动）：本地命中零网络
// （仅 inspect）；miss → pull（RegistryAuth 留空）；pull 传输失败 / 流内
// JSON error 一律含 pull 摘要上抛；inspect 非 NotFound 错误 fail-fast 原样
// 上抛。
func TestEnsureImage_Table(t *testing.T) {
	const ref = "127.0.0.1:5500/func-fn1-dep1"

	cases := []struct {
		name        string
		local       bool     // 本地镜像表预置（inspect 命中）
		inspectErr  error    // 非 nil 时 inspect 弹错
		pullErr     error    // pull 传输错误
		pullStream  string   // pull 流内 JSON error
		wantCalls   []string // 期望调用流水
		wantErr     string   // 空 = 期望成功
		wantPullRef string   // 非 nil 判定时断言 pull 引用
	}{
		{
			name:      "本地命中跳过 pull",
			local:     true,
			wantCalls: []string{"inspect " + ref},
		},
		{
			name:        "miss 触发 pull（零 RegistryAuth）",
			wantCalls:   []string{"inspect " + ref, "pull " + ref},
			wantPullRef: ref,
		},
		{
			name:      "pull 传输失败含摘要",
			pullErr:   errors.New("connection refused"),
			wantCalls: []string{"inspect " + ref, "pull " + ref},
			wantErr:   `docker pull "` + ref + `" failed`,
		},
		{
			name:       "pull 流内错误（引用不存在）含摘要",
			pullStream: `{"errorDetail":{"message":"pull access denied for func-fn1-dep1"}}` + "\n",
			wantCalls:  []string{"inspect " + ref, "pull " + ref},
			wantErr:    "pull access denied",
		},
		{
			name:       "inspect 非 NotFound 错误 fail-fast",
			inspectErr: errors.New("daemon unreachable"),
			wantCalls:  []string{"inspect " + ref},
			wantErr:    `inspect image "` + ref + `"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imgs := newFakeImageClient()
			if tc.local {
				imgs.images[ref] = image.InspectResponse{ID: "sha256:stub"}
			}
			if tc.inspectErr != nil {
				imgs.errs["inspect/"+ref] = []error{tc.inspectErr}
			}
			if tc.pullErr != nil {
				imgs.errs["pull/"+ref] = []error{tc.pullErr}
			}
			if tc.pullStream != "" {
				imgs.pullStreams[ref] = tc.pullStream
			}

			err := ensureImage(context.Background(), imgs, ref)

			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			require.Equal(t, tc.wantCalls, imgs.calls)
			if tc.wantPullRef != "" {
				require.Equal(t, tc.wantPullRef, imgs.lastPullRef)
				require.Empty(t, imgs.lastPullAuth, "RegistryAuth 必须留空 = daemon 侧已登录凭证")
			}
		})
	}
}

// TestDispatchRouting_RegistryMode registry 模式路由语义（四期 4b M7 差异）：
//   - 冷启动无实例不再转发 BuildNode（镜像全局化——任何节点都能 pull），本地
//     spawn + spawn 前 EnsureImage；
//   - 实例亲和照旧转发（实例终生属于 spawn 它的节点，与镜像分布无关）。
func TestDispatchRouting_RegistryMode(t *testing.T) {
	peerResp := &ExecuteResponse{Status: "ok", Response: `{"via":"peer-b"}`}

	newRegistryPool := func(t *testing.T) (*fakeDaemon, *fakeRegistry, *fakeForwarder, *PoolManager) {
		t.Helper()
		d := newFakeDaemon()
		reg := newFakeRegistry()
		fwd := &fakeForwarder{resp: peerResp}
		runner := &fakeRunner{}
		runner.healthy = true
		pool := newTestPool(d, reg, runner, func(c *PoolConfig) {
			c.QueueHeadTimeout = 500 * time.Millisecond
			c.RoutingMode = config.FunctionsRoutingModeRegistry
		})
		pool.SetNodeID("node-a")
		pool.SetForwarder(fwd)
		return d, reg, fwd, pool
	}

	t.Run("无实例+BuildNode=他节点在册→不转发，本地 spawn + spawn 前 EnsureImage", func(t *testing.T) {
		d, reg, fwd, pool := newRegistryPool(t)
		seedNode(reg, "node-b", "http://node-b:9070")

		resp, err := pool.Dispatch(context.Background(), routingReq("node-b"))
		require.NoError(t, err)
		require.Equal(t, "ok", resp.Status)
		require.Zero(t, fwd.count(), "registry 模式冷启动不得转发 BuildNode（本地可 pull）")
		require.Equal(t, 1, d.spawnCount, "冷启动本地 spawn")
		require.Equal(t, []string{dispatchReq().Image}, d.ensures, "spawn 前必须 EnsureImage（部署镜像）")
		require.Len(t, d.pulls, 1, "镜像 miss 触发 pull")
	})

	t.Run("他节点实例在册→实例亲和照旧转发", func(t *testing.T) {
		_, reg, fwd, pool := newRegistryPool(t)
		ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
		seedRouteInstance(t, reg, ref, InstanceRecord{
			InstanceID: "i1", ContainerID: "i1", IP: "10.1.0.1", DeploymentID: "dep1", Node: "node-b",
		})
		seedNode(reg, "node-b", "http://node-b:9070")

		resp, err := pool.Dispatch(context.Background(), routingReq("node-a"))
		require.NoError(t, err)
		require.Contains(t, resp.Response, "peer-b", "响应必须来自转发目标")
		require.Equal(t, 1, fwd.count(), "实例亲和转发不受 routing_mode 影响")
	})
}

// TestDispatch_RegistryModePreSpawnPull 池级 pull 门控与失败语义：
//   - registry 模式冷触发 EnsureImage（本地命中零 pull）；复用热实例不再
//     ensure；
//   - local 模式永不 ensure（镜像不分发，跳过一切 pull 逻辑）；
//   - pull 失败 = spawn 失败现场留存，队首超时错误携带 pull 摘要（不吞）。
func TestDispatch_RegistryModePreSpawnPull(t *testing.T) {
	newCase := func(t *testing.T, routingMode string) (*fakeDaemon, *PoolManager) {
		t.Helper()
		d := newFakeDaemon()
		reg := newFakeRegistry()
		runner := &fakeRunner{}
		runner.healthy = true
		pool := newTestPool(d, reg, runner, func(c *PoolConfig) {
			c.QueueHeadTimeout = 300 * time.Millisecond
			c.RoutingMode = routingMode
		})
		pool.SetNodeID("node-a")
		pool.SetForwarder(&fakeForwarder{})
		return d, pool
	}

	t.Run("registry 模式：冷启动 ensure 一次，热复用不再 ensure", func(t *testing.T) {
		d, pool := newCase(t, config.FunctionsRoutingModeRegistry)
		ctx := context.Background()

		resp, err := pool.Dispatch(ctx, dispatchReq())
		require.NoError(t, err)
		require.Equal(t, "ok", resp.Status)
		require.Equal(t, []string{dispatchReq().Image}, d.ensures)
		require.Len(t, d.pulls, 1, "首次 miss 触发 pull")

		_, err = pool.Dispatch(ctx, dispatchReq())
		require.NoError(t, err)
		require.Len(t, d.ensures, 1, "热实例复用不得再走 EnsureImage")
		require.Equal(t, 1, d.spawnCount)
	})

	t.Run("registry 模式：本地命中零 pull", func(t *testing.T) {
		d, pool := newCase(t, config.FunctionsRoutingModeRegistry)
		d.localImages[dispatchReq().Image] = true

		_, err := pool.Dispatch(context.Background(), dispatchReq())
		require.NoError(t, err)
		require.Equal(t, []string{dispatchReq().Image}, d.ensures)
		require.Empty(t, d.pulls, "本地命中必须跳过 pull")
	})

	t.Run("local 模式永不 pull", func(t *testing.T) {
		d, pool := newCase(t, config.FunctionsRoutingModeLocal)

		_, err := pool.Dispatch(context.Background(), dispatchReq())
		require.NoError(t, err)
		require.Empty(t, d.ensures, "local 模式跳过一切 pull 逻辑")
		require.Empty(t, d.pulls)
		require.Equal(t, 1, d.spawnCount, "local 冷启动行为不变")
	})

	t.Run("registry 模式 pull 失败→队首超时错误携带 pull 摘要", func(t *testing.T) {
		d, pool := newCase(t, config.FunctionsRoutingModeRegistry)
		d.pullErr = fmt.Errorf(`docker pull "torchwood-funcs/func-fn1-dep1" failed: connection refused`)

		_, err := pool.Dispatch(context.Background(), dispatchReq())
		require.Equal(t, codes.ResourceExhausted, status.Code(err))
		msg := status.Convert(err).Message()
		require.Contains(t, msg, "last spawn error")
		require.Contains(t, msg, `docker pull "torchwood-funcs/func-fn1-dep1" failed`,
			"spawn 失败现场（pull 摘要）必须进队首超时消息")
	})

	t.Run("registry 模式 inspect 前置失败同样留现场", func(t *testing.T) {
		d, pool := newCase(t, config.FunctionsRoutingModeRegistry)
		d.ensureErr = errdefs.ErrNotFound

		_, err := pool.Dispatch(context.Background(), dispatchReq())
		require.Equal(t, codes.ResourceExhausted, status.Code(err))
		require.Contains(t, status.Convert(err).Message(), "last spawn error")
	})
}
