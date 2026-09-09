package functionsdispatcher

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
