package cli

import (
	"flag"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// webhookFlagsDecl 声明 webhooks 域动词涉及的旗标（供 presence 测试）。
func webhookFlagsDecl(fs *flag.FlagSet) {
	fs.String("url", "", "")
	fs.String("events", "", "")
	fs.String("status", "", "")
	fs.String("id", "", "")
}

// TestBuildCreateWebhookReq 覆盖创建请求构造：url/events 必填、id/status
// optional（显式传入才生效——presence 经 changed 判定）。
func TestBuildCreateWebhookReq(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		events  string
		id      string
		status  string
		set     map[string]string
		wantErr string
	}{
		{name: "缺 url", events: "auth.*", wantErr: "--url is required"},
		{name: "缺 events", url: "https://hooks.example.com/cb", wantErr: "--events is required"},
		{name: "最小字段", url: "https://hooks.example.com/cb", events: "auth.users.created", wantErr: ""},
		{name: "多订阅串与 optional", url: "https://hooks.example.com/cb",
			events: "auth.users.created, databases.db.collections.col.documents.created",
			id:     "wh-pay", status: "paused",
			set: map[string]string{"status": "paused"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateWebhookReq(newPresenceVerb(t, webhookFlagsDecl, tt.set), tt.url, tt.events, tt.id, tt.status)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.url, req["url"])
			events := req["events"].([]string)
			require.NotEmpty(t, events)
			require.Len(t, events, len(splitList(tt.events)))
			if tt.id != "" {
				require.Equal(t, tt.id, req["id"])
			} else {
				require.NotContains(t, req, "id")
			}
			if tt.status != "" {
				require.Equal(t, tt.status, req["status"])
			} else {
				// status 未显式传入不进请求（proto in 约束允许空 = 缺省 enabled）。
				require.NotContains(t, req, "status")
			}
		})
	}
}

// TestBuildUpdateWebhookReq 覆盖部分更新语义：仅显式传入的字段进请求；
// --events 传入即全量替换（含空切片形态——服务端 min_items 拒绝）。
func TestBuildUpdateWebhookReq(t *testing.T) {
	t.Run("无任何字段", func(t *testing.T) {
		req, err := buildUpdateWebhookReq(newPresenceVerb(t, webhookFlagsDecl, nil), "wh-1", "", "", "")
		require.NoError(t, err)
		require.Equal(t, map[string]any{"webhookId": "wh-1"}, req)
	})
	t.Run("仅 url", func(t *testing.T) {
		req, err := buildUpdateWebhookReq(newPresenceVerb(t, webhookFlagsDecl, map[string]string{"url": "https://n.example.com"}), "wh-1", "https://n.example.com", "", "")
		require.NoError(t, err)
		require.Equal(t, "https://n.example.com", req["url"])
		require.NotContains(t, req, "status")
		require.NotContains(t, req, "events")
	})
	t.Run("events 全量替换", func(t *testing.T) {
		req, err := buildUpdateWebhookReq(newPresenceVerb(t, webhookFlagsDecl, map[string]string{"events": "payments.orders.paid,economy.*"}), "wh-1", "", "payments.orders.paid,economy.*", "")
		require.NoError(t, err)
		require.Equal(t, []string{"payments.orders.paid", "economy.*"}, req["events"])
	})
	t.Run("缺 webhook id", func(t *testing.T) {
		_, err := buildUpdateWebhookReq(newPresenceVerb(t, webhookFlagsDecl, nil), "", "", "", "")
		require.ErrorContains(t, err, "missing webhook-id")
	})
}

// TestWebhooksCmdShape 命令面形状守卫：组名/动词树与九 RPC 对齐，防注册
// 漏项（方法路径常量漂移由 call 面既有测试口径兜底）。
func TestWebhooksCmdShape(t *testing.T) {
	g := NewGlobalFlags("test")
	require.Equal(t, "webhooks", NewWebhooksCmd(g).Name())
}
