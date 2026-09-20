package functions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDispatcherExecutor_BuildCarriesFullPayload 构建链载荷一期定稿（设计
// §0/D14）：DispatcherExecutor 按 BuildSpec 全量组装 BuildRequest——
// project_id/runtime/function_timeout_seconds/env/egress_untrusted/verify
// 齐、zip_base64 内联通道不变（字节级往返一致）。
func TestDispatcherExecutor_BuildCarriesFullPayload(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &config.AppConfig{Functions: &config.Functions{
		Dispatcher: &config.Functions_Dispatcher{Url: srv.URL},
	}}
	exec := NewDispatcherExecutor(cfg)
	zipPath := filepath.Join(t.TempDir(), "code.zip")
	zipBytes := []byte("PK\x03\x04-fake-code")
	require.NoError(t, os.WriteFile(zipPath, zipBytes, 0o600))
	_, err := exec.Build(context.Background(), domainfunctions.BuildSpec{
		ProjectID:              "p1",
		FunctionID:             "fn_1",
		DeploymentID:           "dep_1",
		ZipPath:                zipPath,
		Runtime:                "go-1.26",
		FunctionTimeoutSeconds: 30,
		Env:                    map[string]string{"FOO": "bar"},
		EgressUntrusted:        true,
		Verify:                 true,
	})
	require.NoError(t, err)
	require.Equal(t, "p1", body["project_id"], "project_id 必携（drain 项目语义，D14）")
	require.Equal(t, "fn_1", body["function_id"])
	require.Equal(t, "dep_1", body["deployment_id"])
	require.Equal(t, "go-1.26", body["runtime"], "runtime 必携（D7 对账基准）")
	require.Equal(t, float64(30), body["function_timeout_seconds"], "函数超时必携（旧池 drain 宽限）")
	require.Equal(t, map[string]any{"FOO": "bar"}, body["env"], "函数 variables 必携（验证 spawn，阶段 3 消费）")
	require.Equal(t, true, body["egress_untrusted"])
	require.Equal(t, true, body["verify"])
	require.Equal(t, base64.StdEncoding.EncodeToString(zipBytes), body["zip_base64"],
		"zip base64 内联通道不变（字节级一致）")
}

// TestDispatcherExecutor_BuildReturnsNodeID 四期 4a-1（设计 §4 M5 构建
// 亲和）：Build 解析 BuildResponse.node_id 返回给调用方（app 层落
// deployment.build_node）；构建失败（200 + Error）返回空串。
func TestDispatcherExecutor_BuildReturnsNodeID(t *testing.T) {
	cases := []struct {
		name     string
		respBody string
		wantNode string
		wantErr  bool
	}{
		{name: "成功：node_id 透传", respBody: `{"node_id":"dispatcher-1"}`, wantNode: "dispatcher-1"},
		{name: "成功：旧 dispatcher 无 node_id 字段（兼容空串）", respBody: `{}`, wantNode: ""},
		{name: "失败：Error 非空返回空串", respBody: `{"error":"docker build failed","node_id":"dispatcher-1"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.respBody))
			}))
			defer srv.Close()

			cfg := &config.AppConfig{Functions: &config.Functions{
				Dispatcher: &config.Functions_Dispatcher{Url: srv.URL},
			}}
			exec := NewDispatcherExecutor(cfg)
			zipPath := filepath.Join(t.TempDir(), "code.zip")
			require.NoError(t, os.WriteFile(zipPath, []byte("PK\x03\x04"), 0o600))
			nodeID, err := exec.Build(context.Background(), domainfunctions.BuildSpec{
				ProjectID: "p1", FunctionID: "fn_1", DeploymentID: "dep_1", ZipPath: zipPath,
			})
			if tc.wantErr {
				require.Error(t, err)
				require.Empty(t, nodeID, "构建失败不返回节点 ID（failed 行无亲和语义）")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantNode, nodeID)
		})
	}
}

// TestDispatcherExecutor_ExecuteCarriesConcurrencyAndExecutionID v3 透传链
// 末端断言（docs/design/functions-v3.md §1.5）：domain Execution 的
// Concurrency/ExecutionID 进分发请求体（pool.concurrency / execution_id，
// dispatcher 侧据此固化 InstanceRecord 并发并发 x-tw-execution-id header）。
func TestDispatcherExecutor_ExecuteCarriesConcurrencyAndExecutionID(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","duration_ms":1}`))
	}))
	defer srv.Close()

	cfg := &config.AppConfig{Functions: &config.Functions{
		Dispatcher: &config.Functions_Dispatcher{Url: srv.URL},
	}}
	exec := NewDispatcherExecutor(cfg)
	_, err := exec.Execute(context.Background(), domainfunctions.Execution{
		FunctionID:   "fn_1",
		DeploymentID: "dep_1",
		ProjectID:    "p1",
		Concurrency:  8,
		ExecutionID:  "exe_1",
		Env:          map[string]string{twExecutionTokenEnv: "tok"},
	})
	require.NoError(t, err)
	require.Equal(t, "exe_1", body["execution_id"], "执行 ID 进请求体（dispatcher 转 header）")
	pool, ok := body["pool"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(8), pool["concurrency"], "并发进 pool.concurrency")
	// TW_EXECUTION_TOKEN 仍不进 env（经分发 header 通道，P0.5 既有语义）。
	env, ok := body["env"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, env, twExecutionTokenEnv)
}

// TestDispatcherExecutor_ExecuteCarriesBuildNode 四期 4a-1（设计 §4 M3/M5）：
// domain Execution 的 BuildNode（deployment.build_node 亲和节点）进分发
// 请求体；本阶段 dispatcher 侧不消费（路由是 4a-2），只透传落类型。
func TestDispatcherExecutor_ExecuteCarriesBuildNode(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","duration_ms":1}`))
	}))
	defer srv.Close()

	cfg := &config.AppConfig{Functions: &config.Functions{
		Dispatcher: &config.Functions_Dispatcher{Url: srv.URL},
	}}
	exec := NewDispatcherExecutor(cfg)
	_, err := exec.Execute(context.Background(), domainfunctions.Execution{
		FunctionID:   "fn_1",
		DeploymentID: "dep_1",
		ProjectID:    "p1",
		BuildNode:    "dispatcher-1",
	})
	require.NoError(t, err)
	require.Equal(t, "dispatcher-1", body["build_node"], "build_node 必须随分发请求透传")

	// 无亲和（空串）不发键（omitempty，兼容旧 dispatcher 的请求形状）。
	body = nil
	_, err = exec.Execute(context.Background(), domainfunctions.Execution{
		FunctionID: "fn_1", DeploymentID: "dep_1", ProjectID: "p1",
	})
	require.NoError(t, err)
	require.NotContains(t, body, "build_node", "空 build_node 不发键")
}

// TestDispatcherExecutor_ExecuteCarriesIdentityFields 调用身份贯通（runner
// v5，mlbridge fn-rpc 设计 §2.5）：domain Execution 的 Source/InvokingUserID
// 进分发请求体（source / invoking_user_id；project_id 为既有字段）——
// dispatcher 侧据此经 header 进 runner ctx。client 链路（source=client +
// 真实用户）与 event 链路（source 含 trigger 前缀 + 空 invokingUserId）双覆盖。
func TestDispatcherExecutor_ExecuteCarriesIdentityFields(t *testing.T) {
	cases := []struct {
		name             string
		source           string
		invokingUserID   string
		wantSource       string
		wantInvokingUser string
	}{
		{
			name:             "client 来源：真实调用用户",
			source:           domainfunctions.TriggerSourceClient,
			invokingUserID:   "user-1",
			wantSource:       domainfunctions.TriggerSourceClient,
			wantInvokingUser: "user-1",
		},
		{
			name:             "event 来源：trigger 前缀 + 空 invoking_user_id",
			source:           domainfunctions.TriggerTypeEvent + ":trg-evt-1",
			invokingUserID:   "",
			wantSource:       domainfunctions.TriggerTypeEvent + ":trg-evt-1",
			wantInvokingUser: "", // 空 = 非用户触发（dispatcher 侧不发 header）
		},
		{
			name:             "server 面：字面值 server",
			source:           "server",
			invokingUserID:   "",
			wantSource:       "server",
			wantInvokingUser: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &body))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok","duration_ms":1}`))
			}))
			defer srv.Close()

			cfg := &config.AppConfig{Functions: &config.Functions{
				Dispatcher: &config.Functions_Dispatcher{Url: srv.URL},
			}}
			exec := NewDispatcherExecutor(cfg)
			_, err := exec.Execute(context.Background(), domainfunctions.Execution{
				FunctionID:     "fn_1",
				DeploymentID:   "dep_1",
				ProjectID:      "p1",
				Source:         tc.source,
				InvokingUserID: tc.invokingUserID,
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantSource, body["source"], "source 进分发请求体（dispatcher 转 header）")
			require.Equal(t, tc.wantInvokingUser, body["invoking_user_id"], "invoking_user_id 进分发请求体（空 = dispatcher 侧不发 header）")
			require.Equal(t, "p1", body["project_id"], "project_id 既有字段随行（ctx.projectId 来源）")
		})
	}
}

// TestDispatcherExecutor_ImportImageCarriesPayload 三期阶段 3 真实实现断言
// （设计 §3）：ImportImage 按 ImportImageSpec 全量组装请求——一次性 registry
// 凭证内联单次转发、ExpectedDigest 透传（幂等补拉）、验证 spawn 载荷齐备；
// 响应 digest 即钉死值回传。
func TestDispatcherExecutor_ImportImageCarriesPayload(t *testing.T) {
	pinned := "sha256:" + strings.Repeat("ab", 32)
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"digest":"` + pinned + `"}`))
	}))
	defer srv.Close()

	cfg := &config.AppConfig{Functions: &config.Functions{
		Dispatcher: &config.Functions_Dispatcher{Url: srv.URL},
	}}
	exec := NewDispatcherExecutor(cfg)
	digest, err := exec.ImportImage(context.Background(), domainfunctions.ImportImageSpec{
		ProjectID:              "p1",
		FunctionID:             "fn_1",
		DeploymentID:           "dep_1",
		Reference:              "ghcr.io/acme/greet:v1",
		RegistryUsername:       "user",
		RegistryToken:          "tok",
		ExpectedDigest:         "sha256:expected",
		FunctionTimeoutSeconds: 30,
		Env:                    map[string]string{"FOO": "bar"},
		EgressUntrusted:        true,
	})
	require.NoError(t, err)
	require.Equal(t, pinned, digest, "响应 digest 即钉死值（调用方落 source_ref）")
	require.Equal(t, "p1", body["project_id"])
	require.Equal(t, "fn_1", body["function_id"])
	require.Equal(t, "dep_1", body["deployment_id"])
	require.Equal(t, "ghcr.io/acme/greet:v1", body["reference"])
	require.Equal(t, "user", body["registry_username"], "一次性凭证内联转发（不落库，D8）")
	require.Equal(t, "tok", body["registry_token"])
	require.Equal(t, "sha256:expected", body["expected_digest"], "ExpectedDigest 透传（幂等补拉/复检）")
	require.Equal(t, float64(30), body["function_timeout_seconds"], "函数超时必携（旧池 drain 宽限）")
	require.Equal(t, map[string]any{"FOO": "bar"}, body["env"], "函数 variables 必携（强制契约验证 spawn）")
	require.Equal(t, true, body["egress_untrusted"])
}

// TestDispatcherExecutor_ImportImageErrorMapping 导入失败（host 校验/pull/
// 契约验证）走 200 + Error 业务结果：错误消息透传（第一现场落
// deployment.error）；HTTP 状态码错误（dispatcher 不可达等）走 do 的映射。
func TestDispatcherExecutor_ImportImageErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		respBody   string
		wantErrHas string
		wantCode   codes.Code
	}{
		{
			name:       "business-error-200",
			status:     http.StatusOK,
			respBody:   `{"error":"docker pull \"ghcr.io/a/b:v1\" failed: pull access denied"}`,
			wantErrHas: "pull access denied",
			wantCode:   codes.Unknown,
		},
		{
			name:       "invalid-argument-400",
			status:     http.StatusBadRequest,
			respBody:   `{"error":"image reference host \"192.168.1.5:5000\" is an IP literal: refused"}`,
			wantErrHas: "IP literal",
			wantCode:   codes.InvalidArgument,
		},
		{
			// 镜像缺失类型化错误（rebuild 链路）按码还原（do() 412 通道）。
			name:       "precondition-failed-412",
			status:     http.StatusPreconditionFailed,
			respBody:   `{"error":"deployment image missing \"x\" on this node (rebuild required)"}`,
			wantErrHas: "rebuild required",
			wantCode:   codes.FailedPrecondition,
		},
		{
			name:       "empty-digest-500",
			status:     http.StatusOK,
			respBody:   `{}`,
			wantErrHas: "empty image digest",
			wantCode:   codes.Internal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.respBody))
			}))
			defer srv.Close()

			cfg := &config.AppConfig{Functions: &config.Functions{
				Dispatcher: &config.Functions_Dispatcher{Url: srv.URL},
			}}
			exec := NewDispatcherExecutor(cfg)
			digest, err := exec.ImportImage(context.Background(), domainfunctions.ImportImageSpec{
				ProjectID:    "p1",
				FunctionID:   "fn_1",
				DeploymentID: "dep_1",
				Reference:    "ghcr.io/acme/greet:v1",
			})
			require.Empty(t, digest)
			require.ErrorContains(t, err, tc.wantErrHas)
			require.Equal(t, tc.wantCode, status.Code(err))
		})
	}
}
