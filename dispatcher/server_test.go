package dispatcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T, token string) (*dispatchServer, *fakeDaemon, *PoolManager) {
	t.Helper()
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	return newDispatchServer(pool, d, token), d, pool
}

func postJSON(t *testing.T, srv *dispatchServer, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Tw-Dispatcher-Token", token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestServer_AuthRequiredWhenConfigured(t *testing.T) {
	srv, _, _ := newTestServer(t, "sekrit")

	rec := postJSON(t, srv, "/v1/dispatch/executions", "", dispatchReq())
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = postJSON(t, srv, "/v1/dispatch/executions", "wrong", dispatchReq())
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = postJSON(t, srv, "/v1/dispatch/executions", "sekrit", dispatchReq())
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ExecuteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "ok", resp.Status)
}

func TestServer_NoAuthWhenTokenEmpty(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
	rec := postJSON(t, srv, "/v1/dispatch/executions", "", dispatchReq())
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestServer_ExecuteValidation(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
	// 缺必填字段 → 400。
	req := dispatchReq()
	req.FunctionID = ""
	rec := postJSON(t, srv, "/v1/dispatch/executions", "", req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServer_Build(t *testing.T) {
	srv, d, _ := newTestServer(t, "")
	rec := postJSON(t, srv, "/v1/dispatch/builds", "", BuildRequest{
		ProjectID:    "p1",
		FunctionID:   "fn1",
		DeploymentID: "dep1",
		ZipBase64:    base64.StdEncoding.EncodeToString([]byte("PK\x03\x04-fake")),
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, d.builtImages, 1)

	// 非法 base64 → 400。
	rec = postJSON(t, srv, "/v1/dispatch/builds", "", BuildRequest{
		ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1", ZipBase64: "!!!",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestServer_BuildCarriesOptionsToDaemon 构建链载荷一期定稿（D14/D7/D10）：
// handleBuild 把 BuildRequest 全量字段传入 daemon（fake 全字段断言）。
func TestServer_BuildCarriesOptionsToDaemon(t *testing.T) {
	srv, d, _ := newTestServer(t, "")
	zipBytes := []byte("PK\x03\x04-fake")
	rec := postJSON(t, srv, "/v1/dispatch/builds", "", BuildRequest{
		ProjectID:              "p1",
		FunctionID:             "fn1",
		DeploymentID:           "dep1",
		ZipBase64:              base64.StdEncoding.EncodeToString(zipBytes),
		Runtime:                "go-1.26",
		FunctionTimeoutSeconds: 30,
		Env:                    map[string]string{"FOO": "bar"},
		EgressUntrusted:        true,
		Verify:                 true,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, d.builtImages, 1)
	opts := d.lastBuild
	require.Equal(t, "p1", opts.ProjectID)
	require.Equal(t, "fn1", opts.FunctionID)
	require.Equal(t, "dep1", opts.DeploymentID)
	require.Equal(t, zipBytes, opts.Zip, "zip 须为 base64 解码后的原字节")
	require.Equal(t, "go-1.26", opts.Runtime)
	require.Equal(t, int64(30), opts.FunctionTimeoutSeconds)
	require.Equal(t, map[string]string{"FOO": "bar"}, opts.Env)
	require.True(t, opts.EgressUntrusted)
	require.True(t, opts.Verify)

	// project_id 缺失 → 400（D14：drain 走项目语义，project 必填）。
	rec = postJSON(t, srv, "/v1/dispatch/builds", "", BuildRequest{
		FunctionID: "fn1", DeploymentID: "dep1",
		ZipBase64: base64.StdEncoding.EncodeToString(zipBytes),
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestServer_BuildTriggersDrain D14 顺手修复：FunctionTimeoutSeconds 曾因
// server 侧恒不携带而恒 0、drain 从不触发；载荷补齐后旧池 drain 真正生效
// （构建成功 + function_timeout_seconds>0 → 旧 deployment 的 idle 实例立即
// 回收，新 deployment 实例保留）。
func TestServer_BuildTriggersDrain(t *testing.T) {
	srv, d, pool := newTestServer(t, "")
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	now := time.Now()
	lease := now.Add(time.Minute).UnixMilli()
	require.NoError(t, pool.registry.Save(ctx, ref, InstanceRecord{InstanceID: "old-idle", ContainerID: "old-idle",
		IP: "10.9.0.1", DeploymentID: "dep-old", SpawnedAtMS: now.UnixMilli(), IdleSinceMS: now.UnixMilli(), LeaseUntilMS: lease}))
	d.spawned["old-idle"], d.running["old-idle"] = "10.9.0.1", true

	rec := postJSON(t, srv, "/v1/dispatch/builds", "", BuildRequest{
		ProjectID:              "p1",
		FunctionID:             "fn1",
		DeploymentID:           "dep-new",
		ZipBase64:              base64.StdEncoding.EncodeToString([]byte("PK\x03\x04-fake")),
		FunctionTimeoutSeconds: 1,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	// idle 旧实例在 drain 同步路径回收；同 deployment/其他函数实例不受影响。
	records, err := pool.registry.List(ctx, ref)
	require.NoError(t, err)
	require.Empty(t, records, "旧 deployment 的 idle 实例必须被 drain 回收")
	require.Contains(t, d.stopped, "old-idle")
}

func TestServer_RemoveImage(t *testing.T) {
	srv, d, _ := newTestServer(t, "")
	rec := postJSON(t, srv, "/v1/dispatch/images/remove", "", RemoveImageRequest{
		FunctionID: "fn1", DeploymentID: "dep1",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, d.removedImages, 1)
}

func TestServer_Healthz(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

// healthz 豁免 token 校验（liveness 静态探针，编排健康检查不带凭据），
// 且豁免面仅 GET /healthz：其余方法与 dispatch 端点仍一律 401。
func TestServer_HealthzExemptFromToken(t *testing.T) {
	srv, _, _ := newTestServer(t, "sekrit")

	for _, token := range []string{"", "sekrit"} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		if token != "" {
			req.Header.Set("X-Tw-Dispatcher-Token", token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "token=%q", token)
	}

	// 非 GET 打到 /healthz 不在豁免面内（mux 之前就被 token 校验拦截）。
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// dispatch 端点无 token 依旧 401。
	rec = postJSON(t, srv, "/v1/dispatch/executions", "", dispatchReq())
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// /metrics 与 /healthz 同口径豁免 token：Prometheus 只读观测面，抓取器不带
// 共享密钥；豁免面仅 GET（POST /metrics 与 dispatch 端点仍 401）。
func TestServer_MetricsExemptFromToken(t *testing.T) {
	srv, _, _ := newTestServer(t, "sekrit")

	for _, token := range []string{"", "sekrit"} {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if token != "" {
			req.Header.Set("X-Tw-Dispatcher-Token", token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "token=%q", token)
		require.Contains(t, rec.Body.String(), "torchwood_functions_")
	}

	// 非 GET 打到 /metrics 不在豁免面内（mux 之前就被 token 校验拦截）。
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
