package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖 images/import 端点的 HTTP 面（三期阶段三，设计 §3）：载荷全量
// 传递到 daemon、必填形状校验、错误映射（导入失败 = 200 + Error 业务结果，
// 与 builds 同风格）、token 中间件与旧池 drain。

func TestServer_ImportImage(t *testing.T) {
	srv, d, _ := newTestServer(t, "")
	rec := postJSON(t, srv, "/v1/dispatch/images/import", "", ImportImageRequest{
		ProjectID:              "p1",
		FunctionID:             "fn1",
		DeploymentID:           "dep1",
		Reference:              "ghcr.io/acme/greet:v1",
		RegistryUsername:       "user",
		RegistryToken:          "tok",
		ExpectedDigest:         "sha256:expected",
		FunctionTimeoutSeconds: 30,
		Env:                    map[string]string{"FOO": "bar"},
		EgressUntrusted:        true,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ImportImageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Digest, "成功响应必须携带钉死 digest")
	require.Empty(t, resp.Error)
	require.Len(t, d.imports, 1)

	opts := d.lastImport
	require.Equal(t, "p1", opts.ProjectID)
	require.Equal(t, "fn1", opts.FunctionID)
	require.Equal(t, "dep1", opts.DeploymentID)
	require.Equal(t, "ghcr.io/acme/greet:v1", opts.Reference)
	require.Equal(t, "user", opts.RegistryUsername, "registry 凭证必须原样传递（一次性内联，不落库）")
	require.Equal(t, "tok", opts.RegistryToken)
	require.Equal(t, "sha256:expected", opts.ExpectedDigest)
	require.Equal(t, int64(30), opts.FunctionTimeoutSeconds)
	require.Equal(t, map[string]string{"FOO": "bar"}, opts.Env)
	require.True(t, opts.EgressUntrusted)
}

func TestServer_ImportImageValidation(t *testing.T) {
	cases := []struct {
		name string
		req  ImportImageRequest
	}{
		{"missing-project", ImportImageRequest{FunctionID: "fn1", DeploymentID: "dep1", Reference: "ghcr.io/a/b:v1"}},
		{"missing-function", ImportImageRequest{ProjectID: "p1", DeploymentID: "dep1", Reference: "ghcr.io/a/b:v1"}},
		{"missing-deployment", ImportImageRequest{ProjectID: "p1", FunctionID: "fn1", Reference: "ghcr.io/a/b:v1"}},
		{"missing-reference", ImportImageRequest{ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, d, _ := newTestServer(t, "")
			rec := postJSON(t, srv, "/v1/dispatch/images/import", "", tc.req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Empty(t, d.imports, "形状校验失败不得触达 daemon")
		})
	}
}

// TestServer_ImportImageErrorIsBusinessOutcome 导入失败（host 校验/pull/
// 契约验证）= 部署业务结果：与 builds 同风格 200 + Error（调用方落
// deployment.error），不映射 4xx/5xx。
func TestServer_ImportImageErrorIsBusinessOutcome(t *testing.T) {
	srv, d, _ := newTestServer(t, "")
	d.importErr = errInvalidImport

	rec := postJSON(t, srv, "/v1/dispatch/images/import", "", ImportImageRequest{
		ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1", Reference: "ghcr.io/a/b:v1",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ImportImageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.Digest)
	require.Contains(t, resp.Error, "IP literal", "daemon 错误消息必须透传（第一现场）")
}

// errInvalidImport 模拟 daemon 侧 InvalidArgument（host 校验失败）。
var errInvalidImport = status.Error(codes.InvalidArgument,
	"image reference host \"192.168.1.5:5000\" is an IP literal: refused; allowlist it in functions.image.allowed_registries")

// TestServer_ImportImageRequiresToken token 中间件覆盖新端点（非豁免面）。
func TestServer_ImportImageRequiresToken(t *testing.T) {
	srv, _, _ := newTestServer(t, "sekrit")
	rec := postJSON(t, srv, "/v1/dispatch/images/import", "", ImportImageRequest{})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestServer_ImportImageTriggersDrain 镜像部署换版 drain（与 builds 同语义）：
// 导入成功且 function_timeout_seconds > 0 时旧 deployment 的 idle 实例回收。
func TestServer_ImportImageTriggersDrain(t *testing.T) {
	srv, d, pool := newTestServer(t, "")
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	now := time.Now()
	lease := now.Add(time.Minute).UnixMilli()
	require.NoError(t, pool.registry.Save(ctx, ref, InstanceRecord{InstanceID: "old-idle", ContainerID: "old-idle",
		IP: "10.9.0.1", DeploymentID: "dep-old", SpawnedAtMS: now.UnixMilli(), IdleSinceMS: now.UnixMilli(), LeaseUntilMS: lease}))
	d.spawned["old-idle"], d.running["old-idle"] = "10.9.0.1", true

	rec := postJSON(t, srv, "/v1/dispatch/images/import", "", ImportImageRequest{
		ProjectID:              "p1",
		FunctionID:             "fn1",
		DeploymentID:           "dep-new",
		Reference:              "ghcr.io/acme/greet:v1",
		FunctionTimeoutSeconds: 1,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	records, err := pool.registry.List(ctx, ref)
	require.NoError(t, err)
	require.Empty(t, records, "旧 deployment 的 idle 实例必须被 drain 回收")
	require.Contains(t, d.stopped, "old-idle")
}
