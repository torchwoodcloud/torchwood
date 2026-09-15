package functions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

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
