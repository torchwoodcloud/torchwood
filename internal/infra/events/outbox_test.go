package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

func testEnvelope() domainevents.Envelope {
	return domainevents.Envelope{
		Event:        domainevents.EventDocumentsCreate,
		ProjectID:    "default",
		DatabaseID:   "app",
		CollectionID: "posts",
		DocumentID:   "p1",
		Version:      1,
		CreatedAt:    time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		Data: &databases.Document{
			ID:        "p1",
			Data:      map[string]any{"title": "t"},
			CreatedAt: time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
			Version:   1,
		},
		ACL: domainevents.ACLSnapshot{
			DocumentSecurity: true,
			CollectionPermissions: []databases.Permission{
				{Type: "read", Role: "any"},
			},
			DocumentPermissions: []databases.Permission{
				{Type: "read", Role: "user:u1"},
			},
			DocHasPerms: true,
		},
	}
}

func bigData(n int) map[string]any {
	return map[string]any{"blob": strings.Repeat("x", n)}
}

// TestMarshalEnvelope_IncludesACL：outbox.payload 必须含服务端 acl 快照
// （字符串形式的 permission 数组）。
func TestMarshalEnvelope_IncludesACL(t *testing.T) {
	payload, err := marshalEnvelope(testEnvelope())
	require.NoError(t, err)
	require.False(t, jsonContains(payload, `"truncated":true`))

	var m map[string]any
	require.NoError(t, json.Unmarshal(payload, &m))
	acl, ok := m["acl"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, acl["document_security"])
	require.Equal(t, true, acl["doc_has_perms"])
	require.Equal(t, []any{"read:any"}, acl["collection_permissions"])
	require.Equal(t, []any{"read:user:u1"}, acl["document_permissions"])
	require.Contains(t, m, "data")
}

// TestMarshalEnvelope_TruncatesOversizedData：超 1 MiB 的事件去掉 data、
// 标记 truncated=true，元数据与 acl 保留（防御性截断——写入面 1 MiB 上限
// 已拒超限，本路径只在极端 _acl 场景兜底）。
func TestMarshalEnvelope_TruncatesOversizedData(t *testing.T) {
	ev := testEnvelope()
	ev.Data.Data = bigData(1200 * 1024)
	payload, err := marshalEnvelope(ev)
	require.NoError(t, err)
	require.LessOrEqual(t, len(payload), maxEnvelopeBytes)
	require.True(t, jsonContains(payload, `"truncated":true`))
	require.False(t, jsonContains(payload, `"blob"`))

	var m map[string]any
	require.NoError(t, json.Unmarshal(payload, &m))
	_, ok := m["data"]
	require.False(t, ok, "截断后不得保留 data")
	require.Equal(t, "p1", m["document_id"])
	acl, ok := m["acl"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{"read:user:u1"}, acl["document_permissions"])
}

// TestMarshalEnvelope_KeepsDataUnderLimit：1 MiB 以内保留完整 data。
func TestMarshalEnvelope_KeepsDataUnderLimit(t *testing.T) {
	ev := testEnvelope()
	ev.Data.Data = bigData(500 * 1024)
	payload, err := marshalEnvelope(ev)
	require.NoError(t, err)
	require.False(t, jsonContains(payload, `"truncated":true`))
	require.True(t, jsonContains(payload, `"blob"`))
}

// TestMarshalEnvelope_TruncatesACL：去掉 data 后 acl 仍超限时逐条截断
// permission 数组并保持 truncated=true。
func TestMarshalEnvelope_TruncatesACL(t *testing.T) {
	ev := testEnvelope()
	ev.Data = nil
	for i := 0; i < 90000; i++ {
		ev.ACL.DocumentPermissions = append(ev.ACL.DocumentPermissions,
			databases.Permission{Type: "read", Role: fmt.Sprintf("user:u%d", i)})
	}
	payload, err := marshalEnvelope(ev)
	require.NoError(t, err)
	require.LessOrEqual(t, len(payload), maxEnvelopeBytes)
	require.True(t, jsonContains(payload, `"truncated":true`))

	var m map[string]any
	require.NoError(t, json.Unmarshal(payload, &m))
	acl, ok := m["acl"].(map[string]any)
	require.True(t, ok)
	require.Less(t, len(acl["document_permissions"].([]any)), 90000, "acl 数组必须被截断")
}

// TestPublish_InsertsOutboxRow：Publish 落一行 outbox；topic=集合频道名；
// 本 PR 不标 dispatched_at / published_at。
func TestPublish_InsertsOutboxRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	o := NewEventOutbox(db)

	ev := testEnvelope()
	require.NoError(t, o.Publish(context.Background(), ev))

	rows := queryOutbox(t, db, context.Background())
	require.Len(t, rows, 1)
	require.Equal(t, ev.ProjectID, rows[0].ProjectID)
	require.Equal(t, "databases.app.collections.posts", rows[0].Topic)
	require.Nil(t, rows[0].DispatchedAt)
	require.Nil(t, rows[0].PublishedAt)
	var m map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Payload, &m))
	require.Equal(t, ev.Event, m["event"])
	require.Contains(t, m, "acl")
	require.Contains(t, m, "data")
	require.NotContains(t, m, "transaction_id")
	require.NotEmpty(t, rows[0].EventID)
}

// TestPublish_WithinRunInTx：写路径同一 RunInTx 内调用时复用外层事务
// （事务内即可读到该行；回滚则无行）。
func TestPublish_WithinRunInTx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	o := NewEventOutbox(db)
	ctx := context.Background()

	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := o.Publish(txCtx, testEnvelope()); err != nil {
			return err
		}
		require.Len(t, queryOutbox(t, db, txCtx), 1, "事务内应已可见 outbox 行")
		return fmt.Errorf("simulated failure") // 回滚
	})
	require.Error(t, err)
	require.Len(t, queryOutbox(t, db, ctx), 0, "回滚后不得残留 outbox 行")

	require.NoError(t, db.RunInTx(ctx, func(txCtx context.Context) error {
		return o.Publish(txCtx, testEnvelope())
	}))
	require.Len(t, queryOutbox(t, db, ctx), 1)
}

func queryOutbox(t *testing.T, db *clients.Database, ctx context.Context) []model.DocumentEventsOutbox {
	t.Helper()
	var rows []model.DocumentEventsOutbox
	err := db.Conn(ctx).NewSelect().Model(&rows).Order("created_at ASC").Scan(ctx)
	require.NoError(t, err)
	return rows
}

func jsonContains(payload json.RawMessage, substr string) bool {
	return strings.Contains(string(payload), substr)
}
