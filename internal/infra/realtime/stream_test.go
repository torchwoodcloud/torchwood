package realtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
)

// newStreamEnv 组装 miniredis Stream 测试环境。
func newStreamEnv(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

// TestStreamTransport_XAddFullEnvelope：XADD 载荷是完整信封 JSON（含 acl
// 与 seq），与 outbox.payload 同形；XRANGE 读回往返解码不丢字段。
func TestStreamTransport_XAddFullEnvelope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	client, _ := newStreamEnv(t)
	transport := NewStreamTransport(client)

	ev := testEnvelope()
	ev.Seq = 42
	require.NoError(t, transport.Enqueue(context.Background(), ev))

	entries, err := client.XRange(context.Background(), eventsStream, "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	raw, _ := entries[0].Values["payload"].(string)
	require.NotEmpty(t, raw)

	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	require.Equal(t, ev.EventID, m["event_id"])
	require.Contains(t, m, "acl")
	require.Equal(t, float64(42), m["seq"])

	decoded, err := infraevents.UnmarshalEnvelope([]byte(raw))
	require.NoError(t, err)
	require.Equal(t, ev.EventID, decoded.EventID)
	require.Equal(t, ev.Event, decoded.Event)
	require.Equal(t, ev.Version, decoded.Version)
	require.Equal(t, ev.DocumentID, decoded.DocumentID)
	require.Equal(t, int64(42), decoded.Seq)
	require.Equal(t, ev.ACL.DocumentSecurity, decoded.ACL.DocumentSecurity)
	require.Equal(t, ev.ACL.DocHasPerms, decoded.ACL.DocHasPerms)
	require.Equal(t, ev.ACL.DocumentPermissions, decoded.ACL.DocumentPermissions)
	require.NotNil(t, decoded.Data)
	require.Equal(t, ev.Data.Data, decoded.Data.Data)
}

// TestStreamTransport_Trim：周期裁剪把 Stream 收敛到水位内（近似裁剪）。
func TestStreamTransport_Trim(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	client, _ := newStreamEnv(t)
	transport := NewStreamTransport(client)

	// var 覆写水位（miniredis 的 XTRIM 近似语义按精确处理，直接用小值验证）。
	saved := eventsStreamMaxLen
	eventsStreamMaxLen = 3
	t.Cleanup(func() { eventsStreamMaxLen = saved })

	for i := 0; i < 10; i++ {
		ev := testEnvelope()
		ev.EventID = testEnvelope().EventID
		ev.DocumentID = "d"
		require.NoError(t, transport.Enqueue(context.Background(), ev))
	}
	require.NoError(t, transport.Trim(context.Background()))

	n, err := client.XLen(context.Background(), eventsStream).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, n, int64(3), "Trim 后 Stream 不得超过水位")
}

// TestMarshalEnvelope_RoundTrip：Envelope → JSON → Envelope 无损往返
// （worker 从 outbox.payload 重建信封再 XADD，序列化必须稳定）。
func TestMarshalEnvelope_RoundTrip(t *testing.T) {
	ev := testEnvelope()
	first, err := infraevents.MarshalEnvelope(ev)
	require.NoError(t, err)
	decoded, err := infraevents.UnmarshalEnvelope(first)
	require.NoError(t, err)
	second, err := infraevents.MarshalEnvelope(decoded)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second), "信封往返序列化必须字节一致")
}

// TestStreamTransport_SweepIdleGroups：B6 孤儿消费组治理语义。闲置判定 =
// 位点落后 + 成员全闲置 + PEL 全过时（阈值统一 1h）。断言：
//   - 伪造孤儿组（无成员 / 成员无新鲜证据 + PEL 全过时）被销毁，且组名
//     返回给调用方（日志/指标的数据源）；
//   - 活跃组（成员 idle 未超时 / PEL 未超时）保留；
//   - 位点未落后（贴头部）的组即便无成员也保留。
//
// 时间线（mr.SetTime 推进虚拟时钟，XADD 条目 ID / PEL idle / consumer idle
// 全部随之走）：t0 XADD e1 + 建组；t1=t0+1h 领取；t2=t0+3.5h XCLAIM；
// t4=t0+4h XADD e2（事件流推进头部）+ 扫描（阈值 1h）。
//
// miniredis 限制：XREADGROUP 不维护 consumer lastSeen（XINFO CONSUMERS 恒
// 报 idle=-1，即"无交互证据"），成员新鲜度只能经 XCLAIM 构造（miniredis 的
// XCLAIM 会同刷成员 lastSeen 与 PEL lastDelivery，两者无法单变量隔离——
// PEL 保护因此以"全过时才可删"的反向用例（orphan:stale-pel）覆盖）。
func TestStreamTransport_SweepIdleGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	client, mr := newStreamEnv(t)
	// 与 worker 侧 idleGroupSweeper 同型的类型断言：可选能力不经端口接口暴露。
	transport := NewStreamTransport(client).(*streamTransport)
	ctx := context.Background()

	const idle = time.Hour
	base := time.Now()
	mr.SetTime(base)
	require.NoError(t, transport.Enqueue(ctx, testEnvelope()), "e1（条目 ID 时间戳 = t0）")

	// 用例 A orphan:nobody：0 起步、无消费者、PEL 空、位点落后 → 删。
	require.NoError(t, client.XGroupCreate(ctx, eventsStream, "orphan:nobody", "0").Err())
	// 用例 D orphan:stale-pel：0 起步，t1 领取 1 条不 ACK；此后无任何新鲜
	// 交互证据、PEL 全过时、事件流继续推进头部 → 删。
	require.NoError(t, client.XGroupCreate(ctx, eventsStream, "orphan:stale-pel", "0").Err())
	// 用例 B active:member：t1 领取（不 ACK），t2 XCLAIM 重投——成员 idle
	// 30min、PEL idle 30min → 留（判据"有成员/PEL 未超时"不被删）。
	require.NoError(t, client.XGroupCreate(ctx, eventsStream, "active:member", "0").Err())

	readOne := func(group string) string {
		streams, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: group, Consumer: "c1",
			Streams: []string{eventsStream, ">"},
		}).Result()
		require.NoError(t, err)
		require.Len(t, streams[0].Messages, 1)
		return streams[0].Messages[0].ID
	}

	// t1：orphan:stale-pel 与 active:member 各领 1 条（组间独立投递），不 ACK
	//（XREADGROUP 同时把组位点推进到 e1）。
	mr.SetTime(base.Add(1 * time.Hour))
	readOne("orphan:stale-pel")
	memberMsg := readOne("active:member")

	// t2：active:member 经 XCLAIM 重投在途条目（miniredis 同时刷新成员
	// lastSeen 与 PEL lastDelivery —— 即在线实例 XAUTOCLAIM 重投路径）。
	mr.SetTime(base.Add(3*time.Hour + 30*time.Minute))
	_, err := client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   eventsStream,
		Group:    "active:member",
		Consumer: "c1",
		Messages: []string{memberMsg},
	}).Result()
	require.NoError(t, err)

	// t4 = t0+4h：新事件推进头部（孤儿组位点差自此拉开到 3h+）；新实例
	// 建组（"$" 起步、无成员）——瞬时形态位点贴头部，必须保留。
	mr.SetTime(base.Add(4 * time.Hour))
	require.NoError(t, transport.Enqueue(ctx, testEnvelope()), "e2（事件流推进头部）")
	require.NoError(t, client.XGroupCreate(ctx, eventsStream, "fresh:head", "$").Err())

	destroyed, err := transport.SweepIdleGroups(ctx, idle)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"orphan:nobody", "orphan:stale-pel"}, destroyed,
		"孤儿组必须被销毁且组名返回给调用方")

	groups, err := client.XInfoGroups(ctx, eventsStream).Result()
	require.NoError(t, err)
	var names []string
	for i := range groups {
		names = append(names, groups[i].Name)
	}
	require.ElementsMatch(t, []string{"active:member", "fresh:head"}, names,
		"活跃组（成员/PEL 未超时）与位点未落后组必须保留")
}

// TestStreamTransport_SweepIdleGroups_EmptyStream：Stream 不存在（首次
// XADD 前）视为无可清理，返回空而非错误。
func TestStreamTransport_SweepIdleGroups_EmptyStream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	client, _ := newStreamEnv(t)
	transport := NewStreamTransport(client).(*streamTransport)

	destroyed, err := transport.SweepIdleGroups(context.Background(), time.Hour)
	require.NoError(t, err)
	require.Empty(t, destroyed)
}
