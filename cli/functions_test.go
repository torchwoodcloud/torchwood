package cli

import (
	"flag"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// funcFlagsDecl 声明 functions 域动词涉及的旗标（供 presence 测试；v4：
// spec/entrypoint/runtime/池策略/deployment-id 旗标随执行面外包退役）。
func funcFlagsDecl(fs *flag.FlagSet) {
	fs.Int("timeout-seconds", 0, "")
	fs.Bool("enabled", false, "")
	fs.String("name", "", "")
	fs.String("url", "", "")
	fs.String("target-secret", "", "")
	fs.String("scopes", "", "")
	fs.Bool("async", false, "")
	// 客户端调用面策略三列（P2，设计 §4）。
	fs.Bool("client-callable", false, "")
	fs.Int("client-per-user-limit", 0, "")
	fs.String("client-limit-window", "", "")
	// 触发器。
	fs.String("function-id", "", "")
	fs.String("type", "", "")
	fs.String("response-mode", "", "")
	fs.String("ack-body", "", "")
	fs.String("handshake", "", "")
	fs.Int("body-limit", 0, "")
	fs.String("expr", "", "")
	fs.String("misfire", "", "")
	fs.String("events", "", "")
}

func TestBuildCreateFunctionReq(t *testing.T) {
	tests := []struct {
		name               string
		id                 string
		functionName       string
		targetURL          string
		targetSecret       string
		declaredScopes     string
		timeoutSeconds     int
		enabled            bool
		clientCallable     bool
		clientPerUserLimit int
		clientLimitWindow  string
		set                map[string]string
		wantErr            string
	}{
		{name: "缺 id", functionName: "f", targetURL: "https://fn.example.com", wantErr: "--id is required"},
		{name: "缺 name", id: "f1", targetURL: "https://fn.example.com", wantErr: "--name is required"},
		{name: "缺 url", id: "f1", functionName: "f", wantErr: "--url is required"},
		{name: "最小字段", id: "f1", functionName: "f", targetURL: "https://fn.example.com", wantErr: ""},
		{name: "全字段", id: "f1", functionName: "f", targetURL: "https://fn.example.com", targetSecret: "twfnsec_abc", // #nosec G101 -- 测试夹具伪密钥（非凭证）
			timeoutSeconds: 30, enabled: true, declaredScopes: "assets:write,economy:write",
			set: map[string]string{"timeout-seconds": "30", "enabled": "true"}, wantErr: ""},
		{name: "客户端调用面策略", id: "f1", functionName: "f", targetURL: "https://fn.example.com",
			clientCallable: true, clientPerUserLimit: 15, clientLimitWindow: "hour",
			set: map[string]string{"client-callable": "true", "client-per-user-limit": "15", "client-limit-window": "hour"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateFunctionReq(newPresenceVerb(t, funcFlagsDecl, tt.set), tt.id, tt.functionName, tt.targetURL, tt.targetSecret,
				tt.timeoutSeconds, tt.enabled, tt.clientCallable, tt.clientPerUserLimit, tt.clientLimitWindow, tt.declaredScopes)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.id, req["id"])
			require.Equal(t, tt.functionName, req["name"])
			require.Equal(t, tt.targetURL, req["targetUrl"])
			if tt.targetSecret != "" {
				require.Equal(t, tt.targetSecret, req["targetSecret"])
			} else {
				_, ok := req["targetSecret"]
				require.False(t, ok, "targetSecret 未提供不应设置键: %v", req)
			}
			if scopes := splitList(tt.declaredScopes); len(scopes) > 0 {
				require.Equal(t, scopes, req["declaredScopes"])
			}
			_, wantTimeout := tt.set["timeout-seconds"]
			_, hasTimeout := req["timeoutSeconds"]
			require.Equal(t, wantTimeout, hasTimeout, "timeoutSeconds presence 不匹配: %v", req)
			if wantTimeout {
				require.Equal(t, tt.timeoutSeconds, req["timeoutSeconds"])
			}
			_, wantEnabled := tt.set["enabled"]
			_, hasEnabled := req["enabled"]
			require.Equal(t, wantEnabled, hasEnabled, "enabled presence 不匹配: %v", req)
			if wantEnabled {
				require.Equal(t, tt.enabled, req["enabled"])
			}
			// 客户端调用面策略（P2）：presence + 值保真。
			_, wantCallable := tt.set["client-callable"]
			_, hasCallable := req["clientCallable"]
			require.Equal(t, wantCallable, hasCallable, "clientCallable presence 不匹配: %v", req)
			if wantCallable {
				require.Equal(t, tt.clientCallable, req["clientCallable"])
			}
			_, wantLimit := tt.set["client-per-user-limit"]
			_, hasLimit := req["clientPerUserLimit"]
			require.Equal(t, wantLimit, hasLimit, "clientPerUserLimit presence 不匹配: %v", req)
			if wantLimit {
				require.Equal(t, tt.clientPerUserLimit, req["clientPerUserLimit"])
			}
			_, wantWindow := tt.set["client-limit-window"]
			_, hasWindow := req["clientLimitWindow"]
			require.Equal(t, wantWindow, hasWindow, "clientLimitWindow presence 不匹配: %v", req)
			if wantWindow {
				require.Equal(t, tt.clientLimitWindow, req["clientLimitWindow"])
			}
		})
	}
}

func TestBuildUpdateFunctionReq(t *testing.T) {
	tests := []struct {
		name               string
		functionID         string
		newName            string
		targetURL          string
		timeoutSeconds     int
		enabled            bool
		clientCallable     bool
		clientPerUserLimit int
		clientLimitWindow  string
		set                map[string]string
		wantErr            string
	}{
		{name: "缺 function-id", wantErr: "missing function-id"},
		{name: "仅 name", functionID: "f1", newName: "new", set: map[string]string{"name": "new"}, wantErr: ""},
		{name: "全字段", functionID: "f1", newName: "new", targetURL: "https://new.example.com",
			timeoutSeconds: 60, enabled: false,
			set: map[string]string{"name": "new", "url": "https://new.example.com", "timeout-seconds": "60", "enabled": "false"}, wantErr: ""},
		{name: "客户端调用面策略", functionID: "f1", clientCallable: true, clientPerUserLimit: 20,
			set: map[string]string{"client-callable": "true", "client-per-user-limit": "20"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateFunctionReq(newPresenceVerb(t, funcFlagsDecl, tt.set), tt.functionID, tt.newName, tt.targetURL,
				tt.timeoutSeconds, tt.enabled, tt.clientCallable, tt.clientPerUserLimit, tt.clientLimitWindow)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.functionID, req["functionId"])
			_, wantName := tt.set["name"]
			_, hasName := req["name"]
			require.Equal(t, wantName, hasName, "name presence 不匹配: %v", req)
			if wantName {
				require.Equal(t, tt.newName, req["name"])
			}
			_, wantURL := tt.set["url"]
			_, hasURL := req["targetUrl"]
			require.Equal(t, wantURL, hasURL, "targetUrl presence 不匹配: %v", req)
			if wantURL {
				require.Equal(t, tt.targetURL, req["targetUrl"])
			}
			_, wantTimeout := tt.set["timeout-seconds"]
			_, hasTimeout := req["timeoutSeconds"]
			require.Equal(t, wantTimeout, hasTimeout, "timeoutSeconds presence 不匹配: %v", req)
			if wantTimeout {
				require.Equal(t, tt.timeoutSeconds, req["timeoutSeconds"])
			}
			_, wantEnabled := tt.set["enabled"]
			_, hasEnabled := req["enabled"]
			require.Equal(t, wantEnabled, hasEnabled, "enabled presence 不匹配: %v", req)
			if wantEnabled {
				require.Equal(t, tt.enabled, req["enabled"])
			}
			// 客户端调用面策略（P2）：presence + 值保真。
			_, wantCallable := tt.set["client-callable"]
			_, hasCallable := req["clientCallable"]
			require.Equal(t, wantCallable, hasCallable, "clientCallable presence 不匹配: %v", req)
			if wantCallable {
				require.Equal(t, tt.clientCallable, req["clientCallable"])
			}
			_, wantLimit := tt.set["client-per-user-limit"]
			_, hasLimit := req["clientPerUserLimit"]
			require.Equal(t, wantLimit, hasLimit, "clientPerUserLimit presence 不匹配: %v", req)
			if wantLimit {
				require.Equal(t, tt.clientPerUserLimit, req["clientPerUserLimit"])
			}
			_, wantWindow := tt.set["client-limit-window"]
			_, hasWindow := req["clientLimitWindow"]
			require.Equal(t, wantWindow, hasWindow, "clientLimitWindow presence 不匹配: %v", req)
			if wantWindow {
				require.Equal(t, tt.clientLimitWindow, req["clientLimitWindow"])
			}
		})
	}
}

func TestBuildInvokeReq(t *testing.T) {
	tests := []struct {
		name       string
		functionID string
		input      string
		async      bool
		set        map[string]string
		wantErr    string
	}{
		{name: "缺 input", functionID: "f1", wantErr: "--input is required"},
		{name: "同步最小字段", functionID: "f1", input: `{"a":1}`, wantErr: ""},
		{name: "异步", functionID: "f1", input: `{}`, async: true,
			set: map[string]string{"async": "true"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildInvokeReq(newPresenceVerb(t, funcFlagsDecl, tt.set), tt.functionID, tt.input, tt.async)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.functionID, req["functionId"])
			require.Equal(t, tt.input, req["data"])
			_, wantAsync := tt.set["async"]
			_, hasAsync := req["async"]
			require.Equal(t, wantAsync, hasAsync, "async presence 不匹配: %v", req)
			if wantAsync {
				require.Equal(t, tt.async, req["async"])
			}
		})
	}
}

func TestBuildCreateTriggerReq(t *testing.T) {
	t.Run("http 必填 response-mode", func(t *testing.T) {
		_, err := buildCreateTriggerReq(newPresenceVerb(t, funcFlagsDecl, nil), "f1", "http", "", "", "", 0, "", "", "")
		require.ErrorContains(t, err, "--response-mode is required")
	})
	t.Run("cron 必填 expr", func(t *testing.T) {
		_, err := buildCreateTriggerReq(newPresenceVerb(t, funcFlagsDecl, nil), "f1", "cron", "", "", "", 0, "", "", "")
		require.ErrorContains(t, err, "--expr is required")
	})
	t.Run("event 必填 events", func(t *testing.T) {
		_, err := buildCreateTriggerReq(newPresenceVerb(t, funcFlagsDecl, nil), "f1", "event", "", "", "", 0, "", "", "")
		require.ErrorContains(t, err, "--events is required")
	})
	t.Run("http 全字段", func(t *testing.T) {
		req, err := buildCreateTriggerReq(newPresenceVerb(t, funcFlagsDecl, map[string]string{"body-limit": "1024"}),
			"f1", "http", "async_ack", "OK", "echo", 1024, "", "", "")
		require.NoError(t, err)
		require.Equal(t, "http", req["type"])
		httpCfg := req["http"].(map[string]any)
		require.Equal(t, "async_ack", httpCfg["responseMode"])
		require.Equal(t, "OK", httpCfg["ackBody"])
		require.Equal(t, "echo", httpCfg["handshake"])
		require.Equal(t, int32(1024), httpCfg["bodyLimitBytes"])
	})
	t.Run("event 订阅串", func(t *testing.T) {
		req, err := buildCreateTriggerReq(newPresenceVerb(t, funcFlagsDecl, nil),
			"f1", "event", "", "", "", 0, "", "", "auth.users.created,auth.users.updated")
		require.NoError(t, err)
		require.Equal(t, []string{"auth.users.created", "auth.users.updated"}, req["event"].(map[string]any)["events"])
	})
}
