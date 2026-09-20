package clientgrpc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestMapClientMembershipDoc_InvitedJoinedAt（S11 契约修复）：与 server 面
// 同口径——app 层装配的 invited_at/joined_at 是原生 time.Time，旧
// clientDocTimeField 只认 string → client 面两字段恒空。钉住双形态兼容。
func TestMapClientMembershipDoc_InvitedJoinedAt(t *testing.T) {
	invited := time.Date(2026, 9, 20, 8, 30, 0, 123456789, time.UTC)
	joined := invited.Add(2 * time.Hour)

	// app 层装配形态（原生 time.Time）→ 非 nil 且值正确。
	doc := &databases.Document{
		ID:   "m1",
		Data: map[string]any{"invited_at": invited, "joined_at": joined},
	}
	m := mapClientMembershipDoc(doc)
	require.NotNil(t, m.GetInvitedAt())
	require.NotNil(t, m.GetJoinedAt())
	require.Equal(t, timestamppb.New(invited), m.GetInvitedAt())
	require.Equal(t, timestamppb.New(joined), m.GetJoinedAt())

	// 文档面 JSON 往返形态（string）→ 仍解析成功。
	docStr := &databases.Document{
		ID: "m2",
		Data: map[string]any{
			"invited_at": invited.Format(time.RFC3339Nano),
			"joined_at":  joined.Format(time.RFC3339Nano),
		},
	}
	mStr := mapClientMembershipDoc(docStr)
	require.Equal(t, timestamppb.New(invited), mStr.GetInvitedAt())
	require.Equal(t, timestamppb.New(joined), mStr.GetJoinedAt())

	// 缺失 / 空串 / 不可解析 / 零值 → nil（optional 缺省，不猜 0 值）。
	for name, data := range map[string]map[string]any{
		"missing":      {},
		"empty string": {"invited_at": ""},
		"garbage":      {"invited_at": "not-a-time"},
		"zero time":    {"invited_at": time.Time{}},
	} {
		mm := mapClientMembershipDoc(&databases.Document{ID: "mx", Data: data})
		require.Nil(t, mm.GetInvitedAt(), name)
		require.Nil(t, mm.GetJoinedAt(), name)
	}
}
