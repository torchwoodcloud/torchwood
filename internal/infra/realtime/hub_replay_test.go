package realtime

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

// TestHub_ReplayGateOrdersAndDedups（阶段④ last_seq 门控）：
// 门控期间 Dispatch 的帧积压不投递；EndReplay 时补发批已含的 event_id
// 跳过，其余按序刷入——补发帧先于实时帧、无漏帧。
func TestHub_ReplayGateOrdersAndDedups(t *testing.T) {
	h := NewHub(nil)
	conn := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	ch := testEnvelope().CollectionChannel()

	h.BeginReplay(conn)
	h.Subscribe(ch, conn)

	// 门控期间的两条实时事件（一条与补发批重复）。
	live1 := testEnvelope()
	live1.EventID = "live-1"
	live1.Seq = 11
	live2 := testEnvelope()
	live2.EventID = "live-2"
	live2.Seq = 12
	h.Dispatch(live1)
	h.Dispatch(live2)
	require.Len(t, drainCount2(conn.Send), 0, "门控期间不得投递")

	// 调用方补发（模拟 handler）：先推补发帧（含 live-1 的重复），
	// 再 EndReplay。
	replay := testEnvelope()
	replay.EventID = "replay-1"
	replay.Seq = 5
	require.True(t, conn.TrySend(map[string]any{"type": "event", "payload": replay.ClientPayload(), "x": "replay-1"}, replay.Seq))
	h.EndReplay(conn, map[string]struct{}{"live-1": {}})

	frames := drainCount2(conn.Send)
	require.Len(t, frames, 2, "补发帧 + backlog 去重后仅 live-2")
	require.Equal(t, "replay-1", frames[0]["x"])
	require.Equal(t, "live-2", frames[1]["payload"].(map[string]any)["event_id"])

	// 门控结束后回到实时态。
	live3 := testEnvelope()
	live3.EventID = "live-3"
	live3.Seq = 13
	h.Dispatch(live3)
	frames = drainCount2(conn.Send)
	require.Len(t, frames, 1)
	require.Equal(t, "live-3", frames[0]["payload"].(map[string]any)["event_id"])
}

// TestHub_SlowConsumerDisconnects（阶段④水位断开）：send buffer 满载后
// 下一次入队触发 OnSlow（恰一次，参数 = 最后入队 seq），之后投递跳过
// 该连接；无 OnSlow 的旧语义退化为丢帧续命。
func TestHub_SlowConsumerDisconnects(t *testing.T) {
	h := NewHub(nil)

	// 新语义：带 OnSlow。
	conn := &shared.RealtimeConn{
		ID:           "slow",
		ProjectID:    "default", // 与 testEnvelope 一致（B-1 项目过滤）
		DocPrincipal: databases.Principal{Roles: []string{"users", "user:u1"}},
		Send:         make(chan map[string]any, 2),
	}
	var gotSeq []int64
	conn.OnSlow = func(lastSeq int64) { gotSeq = append(gotSeq, lastSeq) }
	ch := testEnvelope().CollectionChannel()
	h.Subscribe(ch, conn)

	for i := 0; i < 4; i++ {
		ev := testEnvelope()
		ev.EventID = "e" + string(rune('a'+i))
		ev.Seq = int64(i + 1)
		h.Dispatch(ev)
	}
	require.Equal(t, []int64{2}, gotSeq, "满水位（2 帧入队后）触发恰一次，last_seq = 最后成功入队的 seq")
	require.True(t, conn.SlowClosed())
	require.Len(t, drainCount2(conn.Send), 2, "已入队的 2 帧保留在缓冲")

	// 旧语义（无 OnSlow）：丢帧续命，连接不断。
	legacy := newTestConn("legacy", databases.Principal{Roles: []string{"users", "user:u1"}})
	legacy.Send = make(chan map[string]any, 1)
	h.Subscribe(ch, legacy)
	for i := 0; i < 3; i++ {
		ev := testEnvelope()
		ev.EventID = "le"
		ev.Seq = 1
		h.Dispatch(ev)
	}
	require.False(t, legacy.SlowClosed())
	require.Len(t, drainCount2(legacy.Send), 1, "旧语义满水位丢帧（dedup 同 id 只入队首帧）")
}

// drainCount2 收走当前缓冲的全部帧（hub_test.go 的 drain 需要 *testing.T，
// 本文件用无 T 变体）。
func drainCount2(ch chan map[string]any) []map[string]any {
	var out []map[string]any
	for {
		select {
		case f := <-ch:
			out = append(out, f)
		default:
			return out
		}
	}
}
