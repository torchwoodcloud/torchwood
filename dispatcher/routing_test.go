package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---- fakes（路由层表驱动测试；daemon/registry/runner fake 复用 pool_test.go）----

// forwardCall 记录一次转发调用的目标与载荷。
type forwardCall struct {
	target NodeRecord
	req    ExecuteRequest
}

// fakeForwarder 是 nodeForwarder 的内存实现：记录调用，返回可编程响应/错误。
type fakeForwarder struct {
	mu    sync.Mutex
	calls []forwardCall
	resp  *ExecuteResponse
	err   error
}

func (f *fakeForwarder) Forward(_ context.Context, target NodeRecord, req ExecuteRequest) (*ExecuteResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, forwardCall{target: target, req: req})
	resp, err := f.resp, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return resp, nil
	}
	return &ExecuteResponse{Status: "ok", Response: `{"via":"peer"}`, DurationMS: 7}, nil
}

func (f *fakeForwarder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeForwarder) last(t *testing.T) forwardCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.calls, "期望已发生转发调用")
	return f.calls[len(f.calls)-1]
}

// seedRouteInstance 预置一条实例记录（路由决策的实例面布景；与
// registry_redis_integration_test.go 的 seedInstance 同名异形，改名避撞）。
func seedRouteInstance(t *testing.T, reg *fakeRegistry, ref FunctionRef, rec InstanceRecord) {
	t.Helper()
	require.NoError(t, reg.Save(context.Background(), ref, rec))
}

// seedNode 预置一条节点心跳记录（路由决策的节点面布景）。
func seedNode(reg *fakeRegistry, nodeID, url string) {
	reg.nodes[nodeID] = NodeRecord{NodeID: nodeID, URL: url, HbUnixMS: time.Now().UnixMilli()}
}

// routingReq 构造带 BuildNode 的执行请求（p1/fn1/dep1，与 dispatchReq 同底）。
func routingReq(buildNode string) ExecuteRequest {
	req := dispatchReq()
	req.BuildNode = buildNode
	return req
}

// newRoutingPool 组装路由测试池：self=node-a，其余按用例注入。
func newRoutingPool(t *testing.T, d *fakeDaemon, reg *fakeRegistry, fwd *fakeForwarder) *PoolManager {
	t.Helper()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, func(c *PoolConfig) {
		c.QueueHeadTimeout = 500 * time.Millisecond
	})
	pool.SetNodeID("node-a")
	pool.SetForwarder(fwd)
	return pool
}

// TestDispatchRouting_DecisionTable 路由决策表（M3 实例亲和 + M7 local 冷启动
// 语义，docs/design/functions-runtimes-and-sources.md §4）：每例断言落点
// （转发目标 / 本地 spawn）与「转发失败不回落」语义。
func TestDispatchRouting_DecisionTable(t *testing.T) {
	peerResp := &ExecuteResponse{Status: "ok", Response: `{"via":"peer-b"}`}
	unavailable := status.Error(codes.Unavailable, "node-b: connection refused")

	cases := []struct {
		name              string
		instances         []InstanceRecord  // 预置实例（ref p1/fn1）
		nodes             map[string]string // 预置节点心跳（nodeID -> url）
		buildNode         string
		forwarded         bool // 走 DispatchForwarded 入口（带防环标记）
		forwardErr        error
		wantForwardTarget string     // "" = 不得发生转发
		wantForwardErr    codes.Code // 转发发起后的期望错误码（OK = 期望成功）
		wantSpawn         int        // 期望本节点 spawn 次数
		wantRespFromPeer  bool       // 期望响应来自转发目标
	}{
		{
			name:      "本节点实例→本地处理（BuildNode 诱饵不改变亲和）",
			instances: []InstanceRecord{{InstanceID: "i1", ContainerID: "i1", IP: "10.1.0.1", DeploymentID: "dep1", Node: "node-a"}},
			nodes:     map[string]string{"node-b": "http://node-b:9070"},
			buildNode: "node-b",
			wantSpawn: 0,
		},
		{
			name:      "旧记录（Node 空）→按本节点处理（4a-1 兼容口径）",
			instances: []InstanceRecord{{InstanceID: "i1", ContainerID: "i1", IP: "10.1.0.1", DeploymentID: "dep1"}},
			buildNode: "node-b",
			wantSpawn: 0,
		},
		{
			name:              "他节点实例且在册→转发其节点（实例亲和）",
			instances:         []InstanceRecord{{InstanceID: "i1", ContainerID: "i1", IP: "10.1.0.1", DeploymentID: "dep1", Node: "node-b"}},
			nodes:             map[string]string{"node-b": "http://node-b:9070"},
			buildNode:         "node-a",
			wantForwardTarget: "node-b",
			wantRespFromPeer:  true,
		},
		{
			name:      "他节点实例但节点失联→视为孤儿，走冷启动规则（本地回落）",
			instances: []InstanceRecord{{InstanceID: "i1", ContainerID: "i1", IP: "10.1.0.1", DeploymentID: "dep1", Node: "node-b"}},
			buildNode: "",
			wantSpawn: 1,
		},
		{
			name:              "他节点实例转发失败→明确错误不回落本地 spawn",
			instances:         []InstanceRecord{{InstanceID: "i1", ContainerID: "i1", IP: "10.1.0.1", DeploymentID: "dep1", Node: "node-b"}},
			nodes:             map[string]string{"node-b": "http://node-b:9070"},
			forwardErr:        unavailable,
			wantForwardTarget: "node-b",
			wantForwardErr:    codes.Unavailable,
		},
		{
			name:              "无实例+BuildNode=他节点（在册）→转发 BuildNode",
			nodes:             map[string]string{"node-b": "http://node-b:9070"},
			buildNode:         "node-b",
			wantForwardTarget: "node-b",
			wantRespFromPeer:  true,
		},
		{
			name:      "无实例+BuildNode=self→本地 spawn",
			nodes:     map[string]string{"node-a": "http://node-a:9070"},
			buildNode: "node-a",
			wantSpawn: 1,
		},
		{
			name:      "无实例+BuildNode=空→本地 spawn（单机回归形态）",
			buildNode: "",
			wantSpawn: 1,
		},
		{
			name:              "无实例+BuildNode 指向在册但不可达节点→明确错误不回落",
			nodes:             map[string]string{"node-b": "http://node-b:9070"},
			buildNode:         "node-b",
			forwardErr:        unavailable,
			wantForwardTarget: "node-b",
			wantForwardErr:    codes.Unavailable,
		},
		{
			name:      "无实例+BuildNode 指向失联节点（fnnodes 查无）→本地 spawn 回落",
			buildNode: "node-b",
			wantSpawn: 1,
		},
		{
			name:      "forwarded 请求不再转发（防环）：BuildNode 指向他节点也强制本地",
			nodes:     map[string]string{"node-b": "http://node-b:9070"},
			buildNode: "node-b",
			forwarded: true,
			wantSpawn: 1,
		},
		{
			name:              "旧 deployment 的本节点实例不参与路由（deployment 收窄）→转发 BuildNode",
			instances:         []InstanceRecord{{InstanceID: "i1", ContainerID: "i1", IP: "10.1.0.1", DeploymentID: "dep-old", Node: "node-a"}},
			nodes:             map[string]string{"node-b": "http://node-b:9070"},
			buildNode:         "node-b",
			wantForwardTarget: "node-b",
			wantRespFromPeer:  true,
		},
		{
			name:      "旧 deployment 的他节点实例不构成亲和→BuildNode 空则本地 spawn",
			instances: []InstanceRecord{{InstanceID: "i1", ContainerID: "i1", IP: "10.1.0.1", DeploymentID: "dep-old", Node: "node-b"}},
			nodes:     map[string]string{"node-b": "http://node-b:9070"},
			buildNode: "",
			wantSpawn: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon()
			reg := newFakeRegistry()
			fwd := &fakeForwarder{err: tc.forwardErr, resp: peerResp}
			pool := newRoutingPool(t, d, reg, fwd)
			ctx := context.Background()

			ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
			for _, rec := range tc.instances {
				seedRouteInstance(t, reg, ref, rec)
			}
			for id, url := range tc.nodes {
				seedNode(reg, id, url)
			}

			var (
				resp *ExecuteResponse
				err  error
			)
			if tc.forwarded {
				resp, err = pool.DispatchForwarded(ctx, routingReq(tc.buildNode), "node-b")
			} else {
				resp, err = pool.Dispatch(ctx, routingReq(tc.buildNode))
			}

			// 转发断言：目标、次数与载荷。
			if tc.wantForwardTarget == "" {
				require.Zero(t, fwd.count(), "不得发生转发")
			} else {
				require.Equal(t, 1, fwd.count(), "应恰好转发一次")
				call := fwd.last(t)
				require.Equal(t, tc.wantForwardTarget, call.target.NodeID)
				require.Equal(t, tc.nodes[tc.wantForwardTarget], call.target.URL)
				// 载荷透传：deployment 与 BuildNode 原样携带。
				require.Equal(t, tc.buildNode, call.req.BuildNode)
				require.Equal(t, "dep1", call.req.DeploymentID)
			}

			// 落点断言：spawn 次数与结果。
			require.Equal(t, tc.wantSpawn, d.spawnCount, "本节点 spawn 次数不符")
			if tc.wantForwardErr != codes.OK {
				require.Equal(t, tc.wantForwardErr, status.Code(err), "转发失败必须以明确错误收场")
				require.Nil(t, resp)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, resp)
			if tc.wantRespFromPeer {
				require.Equal(t, "ok", resp.Status)
				require.Contains(t, resp.Response, "peer-b", "响应必须来自转发目标")
			}
		})
	}
}

// TestDispatchRouting_SingleNodeRegression 防回归锚：单节点拓扑（无 BuildNode
// 或 BuildNode=self，节点自身在册——真实心跳面形态）下 Dispatch 全链行为与
// 路由层引入前完全一致：冷启动 spawn → 执行 → 复用 → 节点归属固化，路由层
// 零干预（不转发、无额外副作用）。
func TestDispatchRouting_SingleNodeRegression(t *testing.T) {
	for _, tc := range []struct {
		name      string
		buildNode string
	}{
		{name: "无 BuildNode（存量部署行）", buildNode: ""},
		{name: "BuildNode=self", buildNode: "node-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon()
			reg := newFakeRegistry()
			fwd := &fakeForwarder{}
			pool := newRoutingPool(t, d, reg, fwd)
			seedNode(reg, "node-a", "http://node-a:9070") // 本节点心跳在册
			ctx := context.Background()

			req := routingReq(tc.buildNode)
			resp, err := pool.Dispatch(ctx, req)
			require.NoError(t, err)
			require.Equal(t, "ok", resp.Status)
			require.Equal(t, 1, d.spawnCount, "首请求冷启动 spawn")
			require.Zero(t, fwd.count(), "单节点拓扑不得转发")

			resp, err = pool.Dispatch(ctx, req)
			require.NoError(t, err)
			require.Equal(t, "ok", resp.Status)
			require.Equal(t, 1, d.spawnCount, "第二请求复用常驻实例")
			require.Zero(t, fwd.count())
			require.Equal(t, 1, pool.ResidentTotal())

			// 本节点 spawn 固化节点归属（4a-1 语义保持）。
			records, _ := reg.List(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"})
			require.Len(t, records, 1)
			require.Equal(t, "node-a", records[0].Node)
		})
	}
}

// TestDispatchRouting_ForwarderMissing 转发能力未装配（forwarder nil）时，
// 路由需要转发以 FailedPrecondition 明确失败——绝不静默回落本地 spawn。
func TestDispatchRouting_ForwarderMissing(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, func(c *PoolConfig) { c.QueueHeadTimeout = 200 * time.Millisecond })
	pool.SetNodeID("node-a")
	seedNode(reg, "node-b", "http://node-b:9070")

	_, err := pool.Dispatch(context.Background(), routingReq("node-b"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Zero(t, d.spawnCount, "不得静默回落本地 spawn")
}

// ---- httpNodeForwarder 的 HTTP 面单测（httptest 目标节点）----

// fullForwardReq 构造全字段执行请求（载荷透传断言基准）。
func fullForwardReq() ExecuteRequest {
	req := dispatchReq()
	req.BuildNode = "node-b"
	req.ExecutionID = "exec-7"
	req.Source = "trigger"
	req.InvokingUserID = "user-1"
	req.EgressUntrusted = true
	req.Env = map[string]string{"FOO": "bar"}
	req.Pool = PoolPolicy{MaxInstances: 3, IdleTTLSeconds: 60, MaxRequestsPerInstance: 99, Concurrency: 2}
	req.RawBody = []byte(`raw-body`)
	req.TriggerEnvelope = &domainfunctions.TriggerEnvelope{
		Method:   http.MethodPost,
		Path:     "/hook",
		RawQuery: "a=1",
		Headers:  map[string][]string{"X-Test": {"v"}},
	}
	return req
}

// TestNodeForwarder_Passthrough 载荷与 header 完整透传 + 成功响应还原。
func TestNodeForwarder_Passthrough(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	var gotHeaders http.Header
	var gotReq ExecuteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &gotReq))
		writeJSON(w, http.StatusOK, ExecuteResponse{
			Status: "ok", Response: `{"via":"b"}`, DurationMS: 42,
			StatusCode: 201, HTTPHeaders: map[string]string{"X-Fn": "v"}, ResponseB64: "e30=",
		})
	}))
	defer srv.Close()

	f := newNodeForwarder("node-a", "sekrit")
	resp, err := f.Forward(context.Background(), NodeRecord{NodeID: "node-b", URL: srv.URL}, fullForwardReq())
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, forwardExecutionsPath, gotPath)
	// 防环标记 + 共享 token + 内容类型。
	require.Equal(t, "node-a", gotHeaders.Get(forwardedForNodeHeader))
	require.Equal(t, "sekrit", gotHeaders.Get("X-Tw-Dispatcher-Token"))
	require.Equal(t, "application/json", gotHeaders.Get("Content-Type"))
	// 载荷完整透传（JSON 往返后逐字段相等）。
	require.Equal(t, fullForwardReq(), gotReq)
	// 响应原样还原。
	require.Equal(t, &ExecuteResponse{
		Status: "ok", Response: `{"via":"b"}`, DurationMS: 42,
		StatusCode: 201, HTTPHeaders: map[string]string{"X-Fn": "v"}, ResponseB64: "e30=",
	}, resp)
}

// TestNodeForwarder_URLTrailingSlash 目标 URL 带尾斜杠时不得双斜杠拼路径。
func TestNodeForwarder_URLTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, ExecuteResponse{Status: "ok"})
	}))
	defer srv.Close()

	f := newNodeForwarder("node-a", "")
	_, err := f.Forward(context.Background(), NodeRecord{NodeID: "node-b", URL: srv.URL + "/"}, dispatchReq())
	require.NoError(t, err)
	require.Equal(t, forwardExecutionsPath, gotPath)
}

// TestNodeForwarder_ErrorMapping 目标非 2xx → dispatcher writeError 映射之
// 逆映射（与 DispatcherExecutor.do 的还原逻辑同构）。
func TestNodeForwarder_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		body       string
		want       codes.Code
		wantMsg    string
	}{
		{name: "429→ResourceExhausted", httpStatus: http.StatusTooManyRequests, body: `{"error":"queue full"}`, want: codes.ResourceExhausted, wantMsg: "queue full"},
		{name: "504→DeadlineExceeded", httpStatus: http.StatusGatewayTimeout, body: `{"error":"timed out"}`, want: codes.DeadlineExceeded, wantMsg: "timed out"},
		{name: "400→InvalidArgument", httpStatus: http.StatusBadRequest, body: `{"error":"bad request"}`, want: codes.InvalidArgument, wantMsg: "bad request"},
		{name: "401→PermissionDenied", httpStatus: http.StatusUnauthorized, body: `{"error":"nope"}`, want: codes.PermissionDenied, wantMsg: "authentication failed"},
		{name: "500→Internal", httpStatus: http.StatusInternalServerError, body: `{"error":"boom"}`, want: codes.Internal, wantMsg: "boom"},
		{name: "503 无错误体→Internal 兜底消息", httpStatus: http.StatusServiceUnavailable, body: ``, want: codes.Internal, wantMsg: "http 503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.httpStatus)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			f := newNodeForwarder("node-a", "")
			_, err := f.Forward(context.Background(), NodeRecord{NodeID: "node-b", URL: srv.URL}, dispatchReq())
			require.Equal(t, tc.want, status.Code(err))
			require.Contains(t, status.Convert(err).Message(), tc.wantMsg)
			require.Contains(t, status.Convert(err).Message(), "node-b", "错误必须携带目标节点 ID")
		})
	}
}

// TestNodeForwarder_TransportError 目标不可达（进程死/连接拒绝）→ Unavailable，
// 错误携带节点 ID 与 URL（排障第一现场）。
func TestNodeForwarder_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // 立即关闭：后续连接必然被拒

	f := newNodeForwarder("node-a", "")
	_, err := f.Forward(context.Background(), NodeRecord{NodeID: "node-b", URL: url}, dispatchReq())
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "node-b")
	require.Contains(t, status.Convert(err).Message(), url)
}

// TestNodeForwarder_CallerTimeout 调用方超时 → DeadlineExceeded（与
// executeOn 的超时分类同口径；长执行不得被转发层提前截断——无整体
// client.Timeout，只有拨号超时）。
func TestNodeForwarder_CallerTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 慢目标（500ms）远超调用方 ctx 预算（80ms）：转发层必须让 ctx
		// 超时先行收场（固定 sleep 而非 channel 门控，防 srv.Close 与
		// handler 互相等待的死锁）。
		time.Sleep(500 * time.Millisecond)
		writeJSON(w, http.StatusOK, ExecuteResponse{Status: "ok"})
	}))
	defer srv.Close()

	f := newNodeForwarder("node-a", "")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := f.Forward(ctx, NodeRecord{NodeID: "node-b", URL: srv.URL}, dispatchReq())
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
}

// TestServer_ForwardedHeaderForcesLocal HTTP 面防环：带
// X-Tw-Forwarded-For-Node 的请求强制本地池路径（DispatchForwarded），
// 防环标记值不参与判定（任何转发方一律强制本地），且不触发节点转发。
func TestServer_ForwardedHeaderForcesLocal(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	pool.SetNodeID("node-a")
	// 转发能力未装配（nil）：若防环失效（误走 route→forward），请求将以
	// FailedPrecondition 失败而非 200——防环语义由状态码直接暴露。
	srv := newDispatchServer(pool, d, "")
	seedNode(reg, "node-b", "http://node-b:9070")

	raw, err := json.Marshal(routingReq("node-b"))
	require.NoError(t, err)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/dispatch/executions", bytes.NewReader(raw))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(forwardedForNodeHeader, "node-b")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httpReq)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp ExecuteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "ok", resp.Status)
	require.Equal(t, 1, d.spawnCount, "forwarded 请求必须本地 spawn（镜像节点语义）")
}
