package servergrpc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestMapMembershipDoc_InvitedJoinedAt（S11 契约修复）：app 层
// membershipAsDocument 写入 doc.Data 的 invited_at/joined_at 是原生
// time.Time，旧 docTimeField 只认 string（RFC3339Nano）→ 两字段经
// server 面映射恒空。钉住双形态兼容：time.Time 直转、string 解析、
// 缺失/空串/垃圾值 → nil。
func TestMapMembershipDoc_InvitedJoinedAt(t *testing.T) {
	invited := time.Date(2026, 9, 20, 8, 30, 0, 123456789, time.UTC)
	joined := invited.Add(2 * time.Hour)

	// app 层装配形态（原生 time.Time）→ 非 nil 且值正确。
	doc := &databases.Document{
		ID:   "m1",
		Data: map[string]any{"invited_at": invited, "joined_at": joined},
	}
	m := mapMembershipDoc(doc)
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
	mStr := mapMembershipDoc(docStr)
	require.Equal(t, timestamppb.New(invited), mStr.GetInvitedAt())
	require.Equal(t, timestamppb.New(joined), mStr.GetJoinedAt())

	// 缺失 / 空串 / 不可解析 / 零值 → nil（optional 缺省，不猜 0 值）。
	for name, data := range map[string]map[string]any{
		"missing":      {},
		"empty string": {"invited_at": ""},
		"garbage":      {"invited_at": "not-a-time"},
		"zero time":    {"invited_at": time.Time{}},
	} {
		mm := mapMembershipDoc(&databases.Document{ID: "mx", Data: data})
		require.Nil(t, mm.GetInvitedAt(), name)
		require.Nil(t, mm.GetJoinedAt(), name)
	}
}
