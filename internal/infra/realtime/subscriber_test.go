package realtime

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

// newSubscriberEnv 组装 subscriber 集成测试环境：真实 Postgres（迁移含
// outbox）+ miniredis Stream（v2.38 支持 XADD/XGROUP/XREADGROUP(BLOCK)/
// XACK/XAUTOCLAIM/XTRIM 全集——经模块源码核验，无需真实 Redis）。
func newSubscriberEnv(t *testing.T) (*Subscriber, *Hub, *redis.Client, *clients.Database) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	hub := NewHub(nil)
	sub := NewRealtimeSubscriber(client, db, hub, nil)
	return sub, hub, client, db
}

// waitGroupReady 等待 n 个消费组在 Stream 上就位（替代旧 Pub/Sub 的
// waitSubscribed：订阅就绪 = 组存在）。
func waitGroupReady(t *testing.T, client *redis.Client, n int) {
	t.Helper()
	testutil.Eventually(t, 5*time.Second, func() bool {
		groups, err := client.XInfoGroups(context.Background(), eventsStream).Result()
		return err == nil && len(groups) >= n
	})
}

// waitFrame 等待 Hub 扇出的帧（带超时）。
func waitFrame(t *testing.T, conn *Conn) map[string]any {
	t.Helper()
	select {
	case frame := <-conn.Send:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for hub frame")
		return nil
	}
}

// drainCount 收走当前缓冲的全部帧并计数。
func drainCount(ch chan map[string]any) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

// runSubscriber 以指定实例 ID 启动消费循环（同一实例 ID 可模拟重启复组）。
func runSubscriber(t *testing.T, s *Subscriber, instance string) context.CancelFunc {
	t.Helper()
	s.instance = instance
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return cancel
}

// TestSubscriber_GroupsEachConsumeAll（B3 核心语义）：两个实例（两组）
// 各自消费全量——一次 XADD，两个 Hub 的订阅者都收到；published_at 落库；
// 无 read 权的订阅者被 acl 过滤（沿用旧 DispatchUsesFullEnvelopeACL 断言面）。
func TestSubscriber_GroupsEachConsumeAll(t *testing.T) {
	sub1, hub1, client, db := newSubscriberEnv(t)
	hub2 := NewHub(nil)
	sub2 := NewRealtimeSubscriber(client, db, hub2, nil)

	ctx := context.Background()
	ev := testEnvelope()
	require.NoError(t, infraevents.NewEventOutbox(db).Publish(ctx, ev))

	r1 := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	r2 := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	stranger := newTestConn("u2", databases.Principal{Roles: []string{"users", "user:u2"}})
	hub1.Subscribe(ev.CollectionChannel(), r1)
	hub2.Subscribe(ev.CollectionChannel(), r2)
	hub2.Subscribe(ev.CollectionChannel(), stranger)

	runSubscriber(t, sub1, "inst-a")
	runSubscriber(t, sub2, "inst-b")
	waitGroupReady(t, client, 2)

	require.NoError(t, NewStreamTransport(client).Enqueue(ctx, ev))

	frame1 := waitFrame(t, r1)
	frame2 := waitFrame(t, r2)
	require.Equal(t, ev.EventID, frame1["payload"].(map[string]any)["event_id"])
	require.Equal(t, ev.EventID, frame2["payload"].(map[string]any)["event_id"])
	require.NotContains(t, frame1["payload"].(map[string]any), "acl")
	require.Equal(t, 0, drainCount(stranger.Send), "无 read 权不得收到事件")

	require.Eventually(t, func() bool {
		row := outboxRow(t, db, ctx, ev.EventID)
		return row != nil && row.PublishedAt != nil
	}, 5*time.Second, 50*time.Millisecond)
}

// TestSubscriber_AckPositionsNoRedelivery（位点语义）：XACK 后同组重启
// 不重投——同实例 ID 重建消费循环，历史条目不重复扇出，仅新条目到达。
func TestSubscriber_AckPositionsNoRedelivery(t *testing.T) {
	sub, hub, client, db := newSubscriberEnv(t)
	ctx := context.Background()

	ev1 := testEnvelope()
	require.NoError(t, infraevents.NewEventOutbox(db).Publish(ctx, ev1))
	transport := NewStreamTransport(client)

	r := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	hub.Subscribe(ev1.CollectionChannel(), r)

	runSubscriber(t, sub, "inst-restart")
	waitGroupReady(t, client, 1)
	require.NoError(t, transport.Enqueue(ctx, ev1))
	require.Equal(t, ev1.EventID, waitFrame(t, r)["payload"].(map[string]any)["event_id"])
	require.Eventually(t, func() bool {
		return outboxAcked(t, client, "inst-restart")
	}, 5*time.Second, 50*time.Millisecond, "XACK 后组位点必须推进")

	// 「重启」：同实例 ID、同 Hub（客户端保持订阅）起新消费循环。
	sub2 := NewRealtimeSubscriber(client, db, hub, nil)
	runSubscriber(t, sub2, "inst-restart")
	waitGroupReady(t, client, 1)

	// 新事件正常到达，历史事件（ev1）不重投。
	ev2 := testEnvelope()
	ev2.EventID = "evt-2"
	ev2.DocumentID = "p2"
	require.NoError(t, infraevents.NewEventOutbox(db).Publish(ctx, ev2))
	require.NoError(t, transport.Enqueue(ctx, ev2))
	frame := waitFrame(t, r)
	require.Equal(t, ev2.EventID, frame["payload"].(map[string]any)["event_id"])
	require.Equal(t, 0, drainCount(r.Send), "XACK 过的历史条目不得重投")
}

// TestSubscriber_PELClaimRedelivers：消费（扇出）后未 XACK 的条目留在
// PEL——idle 超过 claimMinIdle 后被 XAUTOCLAIM 重投（崩溃恢复语义）；
// Hub 的 event_id 去重窗口内不二次扇出（重复吸收）。
func TestSubscriber_PELClaimRedelivers(t *testing.T) {
	sub, hub, client, db := newSubscriberEnv(t)
	ctx := context.Background()

	ev := testEnvelope()
	require.NoError(t, infraevents.NewEventOutbox(db).Publish(ctx, ev))

	r := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	hub.Subscribe(ev.CollectionChannel(), r)

	// 手工以同名组消费但不 XACK：制造 PEL 挂起条目。
	require.NoError(t, client.XGroupCreateMkStream(ctx, eventsStream, "inst-pel", "$").Err())
	require.NoError(t, NewStreamTransport(client).Enqueue(ctx, ev))
	testutil.Eventually(t, 5*time.Second, func() bool {
		res, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: "inst-pel", Consumer: "inst-pel",
			Streams: []string{eventsStream, ">"}, Count: 1,
		}).Result()
		return err == nil && len(res) == 1 && len(res[0].Messages) == 1
	})

	// 缩短认领窗口（var 覆写，同 queue.claimMinIdle 先例）。
	saved := claimMinIdle
	claimMinIdle = 50 * time.Millisecond
	t.Cleanup(func() { claimMinIdle = saved })

	runSubscriber(t, sub, "inst-pel")
	// 同组消费者：claimStale 认领 PEL 条目 → Dispatch（本进程首次见到该
	// 事件，扇出一次）。
	frame := waitFrame(t, r)
	require.Equal(t, ev.EventID, frame["payload"].(map[string]any)["event_id"])
	require.Equal(t, 0, drainCount(r.Send), "去重窗口内不得二次扇出")

	require.Eventually(t, func() bool {
		row := outboxRow(t, db, ctx, ev.EventID)
		return row != nil && row.PublishedAt != nil
	}, 5*time.Second, 50*time.Millisecond)
}

// TestSubscriber_NewGroupStartsAtDollar：新实例（新组）从 "$" 起步——
// 历史条目不回放（客户端断线窗口由 last_seq 重放补齐，不在 Stream 层）。
func TestSubscriber_NewGroupStartsAtDollar(t *testing.T) {
	sub, hub, client, db := newSubscriberEnv(t)
	ctx := context.Background()
	transport := NewStreamTransport(client)

	ev1 := testEnvelope()
	require.NoError(t, infraevents.NewEventOutbox(db).Publish(ctx, ev1))
	// inst-old 先消费掉 ev1。
	runSubscriber(t, sub, "inst-old")
	waitGroupReady(t, client, 1)
	r := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	hub.Subscribe(ev1.CollectionChannel(), r)
	require.NoError(t, transport.Enqueue(ctx, ev1))
	require.Equal(t, ev1.EventID, waitFrame(t, r)["payload"].(map[string]any)["event_id"])

	// 新实例（新组）+ 新订阅者：不得收到 ev1 的回放。
	sub2, hub2, _, _ := newSubscriberEnvShared(t, client, db)
	r2 := newTestConn("u2", databases.Principal{Roles: []string{"users", "user:u1"}})
	hub2.Subscribe(ev1.CollectionChannel(), r2)
	runSubscriber(t, sub2, "inst-new")
	waitGroupReady(t, client, 2)
	time.Sleep(300 * time.Millisecond) // 给潜在错误回放留窗口
	require.Equal(t, 0, drainCount(r2.Send), "新组不得回放 $ 之前的历史条目")

	// 新事件两组都收到。
	ev2 := testEnvelope()
	ev2.EventID = "evt-new"
	ev2.DocumentID = "p9"
	require.NoError(t, infraevents.NewEventOutbox(db).Publish(ctx, ev2))
	require.NoError(t, transport.Enqueue(ctx, ev2))
	require.Equal(t, ev2.EventID, waitFrame(t, r2)["payload"].(map[string]any)["event_id"])
}

// newSubscriberEnvShared 用共享 client/db 组装第二实例环境。
func newSubscriberEnvShared(t *testing.T, client *redis.Client, db *clients.Database) (*Subscriber, *Hub, *redis.Client, *clients.Database) {
	t.Helper()
	hub := NewHub(nil)
	return NewRealtimeSubscriber(client, db, hub, nil), hub, client, db
}

func outboxRow(t *testing.T, db *clients.Database, ctx context.Context, eventID string) *model.DocumentEventsOutbox {
	t.Helper()
	var row model.DocumentEventsOutbox
	err := db.Conn(ctx).NewSelect().Model(&row).Where("event_id = ?", eventID).Scan(ctx)
	if err != nil {
		return nil
	}
	return &row
}

// outboxAcked 报告组内是否已无 pending（XACK 完成）。
func outboxAcked(t *testing.T, client *redis.Client, group string) bool {
	t.Helper()
	groups, err := client.XInfoGroups(context.Background(), eventsStream).Result()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g.Name == group {
			return g.Pending == 0
		}
	}
	return false
}
