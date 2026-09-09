package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
)

func testEnvelope() Envelope {
	return Envelope{
		EventID:      "01JTESTID",
		Event:        EventDocumentsUpdate,
		ProjectID:    "default",
		DatabaseID:   "app",
		CollectionID: "posts",
		DocumentID:   "p1",
		Version:      4,
		CreatedAt:    time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		Truncated:    false,
		Data: &databases.Document{
			ID:        "p1",
			Data:      map[string]any{"title": "t"},
			CreatedAt: time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
			Version:   4,
			Permissions: []databases.Permission{
				{Type: "read", Role: "user:u1"},
				{Type: "update", Role: "user:u1"},
			},
		},
		ACL: ACLSnapshot{
			DocumentSecurity: true,
			CollectionPermissions: []databases.Permission{
				{Type: "read", Role: "any"},
				{Type: "create", Role: "users"},
			},
			DocumentPermissions: []databases.Permission{
				{Type: "read", Role: "user:u1"},
			},
			DocHasPerms: true,
		},
	}
}

// TestClientPayload_NoACL：出站帧剥掉 acl，不得含 collection_permissions。
func TestClientPayload_NoACL(t *testing.T) {
	payload := testEnvelope().ClientPayload()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NotContains(t, string(b), "acl")
	require.NotContains(t, string(b), "collection_permissions")
	require.NotContains(t, string(b), "document_permissions")

	require.Equal(t, "01JTESTID", payload["event_id"])
	require.Equal(t, EventDocumentsUpdate, payload["event"])
	require.Equal(t, "default", payload["project_id"])
	require.Equal(t, "app", payload["database_id"])
	require.Equal(t, "posts", payload["collection_id"])
	require.Equal(t, "p1", payload["document_id"])
	require.Equal(t, int64(4), payload["version"])
	require.NotContains(t, payload, "transaction_id")
	require.Equal(t, "2026-08-15T12:00:00Z", payload["created_at"])
	require.Equal(t, false, payload["truncated"])
}

// TestClientPayload_DataShape：data 与 REST Document 同形，含顶层 version，
// 无 _ 系统列。
func TestClientPayload_DataShape(t *testing.T) {
	payload := testEnvelope().ClientPayload()
	data, ok := payload["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "p1", data["id"])
	require.Equal(t, map[string]any{"title": "t"}, data["data"])
	require.Equal(t, int64(4), data["version"])
	require.Equal(t, []string{"read:user:u1", "update:user:u1"}, data["permissions"])
	require.NotContains(t, data, "_id")
	require.NotContains(t, data, "_version")
	require.NotContains(t, data, "_tenant")
	require.NotContains(t, data, "Tenant")
}

// TestClientPayload_DeleteOmitsData：delete 事件无 data 键。
func TestClientPayload_DeleteOmitsData(t *testing.T) {
	ev := testEnvelope()
	ev.Data = nil
	payload := ev.ClientPayload()
	_, ok := payload["data"]
	require.False(t, ok)
}

// TestDocumentPayload_NoSystemColumns：payload 不含 _ 前缀系统列。
func TestDocumentPayload_NoSystemColumns(t *testing.T) {
	doc := &databases.Document{
		ID:        "p1",
		Tenant:    42,
		Data:      map[string]any{"title": "t"},
		Version:   4,
		CreatedAt: time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(DocumentPayload(doc))
	require.NoError(t, err)
	s := string(b)
	require.NotContains(t, s, "_tenant")
	require.NotContains(t, s, "_created_at")
	require.NotContains(t, s, "CreatedBy")
}

// TestChannels：集合 / 文档频道名按设计拼接，documentId 可含 . : -。
func TestChannels(t *testing.T) {
	ev := testEnvelope()
	require.Equal(t, "databases.app.collections.posts", ev.CollectionChannel())
	ev.DocumentID = "p.1:2-3"
	require.Equal(t, "databases.app.collections.posts.documents.p.1:2-3", ev.DocumentChannel())
}

// TestVisibleTo：按快照 ACL 过滤；系统主体旁路。
func TestVisibleTo(t *testing.T) {
	acl := ACLSnapshot{
		DocumentSecurity: true,
		DocumentPermissions: []databases.Permission{
			{Type: "read", Role: "user:u1"},
		},
		DocHasPerms: true,
	}
	require.True(t, VisibleTo(acl, databases.Principal{Roles: []string{"users", "user:u1"}}))
	require.False(t, VisibleTo(acl, databases.Principal{Roles: []string{"users", "user:u2"}}))
	require.True(t, VisibleTo(acl, databases.Principal{PlatformAdmin: true}))
	require.True(t, VisibleTo(acl, databases.SystemPrincipal))
}

// TestClientPayload_EconomyCarriesVersion (P1-14)：经济事件 payload 带
// version（对齐文档事件），客户端可按其判序。
func TestClientPayload_EconomyCarriesVersion(t *testing.T) {
	e := Envelope{
		EventID:   "evt_v1",
		Event:     "payments.orders.paid",
		ProjectID: "p1",
		Domain:    "payments",
		Channel:   "accounts.u1",
		CreatedAt: time.Now(),
		Version:   time.Now().UnixNano(),
		Attrs:     map[string]any{"order_id": "o1"},
	}
	payload := e.ClientPayload()
	v, ok := payload["version"]
	if !ok {
		t.Fatal("经济事件 payload 必须含 version")
	}
	if got, ok := v.(int64); !ok || got <= 0 {
		t.Fatalf("version 必须为正 int64，got %v (%T)", v, v)
	}
}
