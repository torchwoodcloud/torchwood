package events

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
)

// TestEconomyEnvelopeRoundTrip 验证经济事件信封（domain/channel/attrs）
// 与 outbox.payload / Redis Stream 条目同形无损往返（v3 设计 §5.1）。
func TestEconomyEnvelopeRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ev := domainevents.Envelope{
		EventID:   "evt_1",
		Event:     "payments.orders.paid",
		ProjectID: "p1",
		Domain:    "payments",
		Channel:   "accounts.u1",
		CreatedAt: now,
		Attrs: map[string]any{
			"order_id":     "o1",
			"user_id":      "u1",
			"amount":       int64(1999),
			"currency":     "USD",
			"status":       "paid",
			"purpose_kind": "topup",
		},
	}
	payload, err := MarshalEnvelope(ev)
	require.NoError(t, err)

	decoded, err := UnmarshalEnvelope(payload)
	require.NoError(t, err)
	require.Equal(t, ev.EventID, decoded.EventID)
	require.Equal(t, ev.Event, decoded.Event)
	require.Equal(t, ev.ProjectID, decoded.ProjectID)
	require.Equal(t, ev.Domain, decoded.Domain)
	require.Equal(t, ev.Channel, decoded.Channel)
	require.Equal(t, ev.CreatedAt.Unix(), decoded.CreatedAt.Unix())
	require.Equal(t, ev.Attrs["order_id"], decoded.Attrs["order_id"])
	// 金额在 Go 全链路 int64；JSON 解码到 map[string]any 呈现 float64 仅是
	// 序列化载体的解码形态（Attrs 只用于透传扇出，不参与任何金额计算）。
	require.EqualValues(t, ev.Attrs["amount"], decoded.Attrs["amount"])
	require.Equal(t, ev.Attrs["currency"], decoded.Attrs["currency"])
	require.True(t, decoded.IsEconomy())

	// 经济事件 payload 无 acl / 无文档字段（隐私与形状，设计 §5.1）。
	require.NotContains(t, string(payload), "\"acl\"")
	require.NotContains(t, string(payload), "\"database_id\"")
	require.NotContains(t, string(payload), "\"collection_id\"")
	require.Contains(t, string(payload), "\"domain\":\"payments\"")
}

// TestDocumentEnvelopeUnchangedWithEconomyFieldsPresent 验证文档事件
// （Domain 为空）序列化形状不因经济扩展改变：无 domain/channel 键，
// 仍含 database/collection/version/acl。
func TestDocumentEnvelopeUnchangedWithEconomyFieldsPresent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ev := domainevents.Envelope{
		EventID:      "evt_2",
		Event:        domainevents.EventDocumentsCreate,
		ProjectID:    "p1",
		DatabaseID:   "db1",
		CollectionID: "coll1",
		DocumentID:   "doc1",
		Version:      3,
		CreatedAt:    now,
	}
	payload, err := MarshalEnvelope(ev)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "\"domain\"")
	require.NotContains(t, string(payload), "\"channel\"")
	require.Contains(t, string(payload), "\"database_id\":\"db1\"")
	require.Contains(t, string(payload), "\"acl\"")

	decoded, err := UnmarshalEnvelope(payload)
	require.NoError(t, err)
	require.False(t, decoded.IsEconomy())
	require.Empty(t, decoded.Domain)
	require.Empty(t, decoded.Channel)
	require.Nil(t, decoded.Attrs)
}
