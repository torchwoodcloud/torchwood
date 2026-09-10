package events

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/uptrace/bun/driver/pgdriver"
)

// TestOutbox_SeqMonotonic（阶段④包 A，B1 顺序承诺）：
// seq 随 INSERT 单调递增；多事件 Publish 后行序 = 插入序。
func TestOutbox_SeqMonotonic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	o := NewEventOutbox(db)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		ev := testEnvelope()
		ev.DocumentID = fmt.Sprintf("p%d", i)
		require.NoError(t, o.Publish(ctx, ev))
	}

	rows := queryOutbox(t, db, ctx)
	require.Len(t, rows, 5)
	for i := 1; i < len(rows); i++ {
		require.Greater(t, rows[i].Seq, rows[i-1].Seq, "seq 必须严格单调（分配序）")
	}
}

// TestOutbox_SeqGapFromRollback（B1：seq 有空洞 = 回滚事务，不丢事件）：
// 一个回滚的事务消耗一个 identity 值——提交后续事件时 seq 跳号，
// 但已提交事件一个不少。
func TestOutbox_SeqGapFromRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	o := NewEventOutbox(db)
	ctx := context.Background()

	require.NoError(t, o.Publish(ctx, testEnvelope()))
	first := queryOutbox(t, db, ctx)[0]

	// 回滚事务：插入一行（消耗 identity）后整体回滚。
	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := o.Publish(txCtx, testEnvelope()); err != nil {
			return err
		}
		return fmt.Errorf("simulated rollback")
	})
	require.Error(t, err)

	require.NoError(t, o.Publish(ctx, testEnvelope()))
	rows := queryOutbox(t, db, ctx)
	require.Len(t, rows, 2, "回滚事务不得产生可见事件")
	require.GreaterOrEqual(t, rows[1].Seq-first.Seq, int64(2),
		"回滚消耗的 identity 必须形成空洞（seq 跳号 ≥2）")
}

// TestOutbox_NotifyWakesListener（阶段④ NOTIFY 原型验证 + 数字）：
// Publish 提交 → 同事务 pg_notify('tw_outbox',”) → worker 专属 LISTEN
// 连接（pgdriver.Listener）在毫秒级收到唤醒。N 次取平均/最大值进测试日志，
// 供实施报告引用。
func TestOutbox_NotifyWakesListener(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	o := NewEventOutbox(db)
	ctx := context.Background()

	ln := pgdriver.NewListener(db.DB)
	t.Cleanup(func() { _ = ln.Close() })
	require.NoError(t, ln.Listen(ctx, outboxNotifyChannel))
	notify := ln.Channel(pgdriver.WithChannelSize(16))
	// 预热：Listener.Channel 启动后有一个 ping 周期，先排掉 ping 通知。
	require.NoError(t, o.Publish(ctx, testEnvelope()))
	drainOne(t, notify)

	const n = 20
	var total time.Duration
	var worst time.Duration
	for i := 0; i < n; i++ {
		ev := testEnvelope()
		ev.DocumentID = fmt.Sprintf("p%d", i)
		start := time.Now()
		require.NoError(t, o.Publish(ctx, ev))
		drainOne(t, notify)
		elapsed := time.Since(start)
		total += elapsed
		if elapsed > worst {
			worst = elapsed
		}
	}
	avg := total / n
	t.Logf("NOTIFY 唤醒延迟（commit → LISTEN 收到，n=%d）：avg=%s max=%s", n, avg, worst)
	require.Less(t, avg, time.Second, "NOTIFY 唤醒平均延迟应远低于 1s（兜底轮询为 5s）")
}

// drainOne 等待一条 tw_outbox 通知（ping 通知已由 Channel 内部过滤，
// 到达这里的都是业务信号），超时失败。
func drainOne(t *testing.T, ch <-chan pgdriver.Notification) {
	t.Helper()
	select {
	case _, ok := <-ch:
		require.True(t, ok, "listener channel 不得提前关闭")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tw_outbox notification")
	}
}

// TestPublish_TransactionIDFromContext（阶段④包 A，§4.8）：
// ctx 带 transaction_id 时落进 payload；不带时 payload 无该键。
func TestPublish_TransactionIDFromContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	o := NewEventOutbox(db)
	ctx := context.Background()

	ctxTx := domainevents.WithTransactionID(ctx, "tx_01")
	require.NoError(t, o.Publish(ctxTx, testEnvelope()))
	require.NoError(t, o.Publish(ctx, testEnvelope()))

	rows := queryOutbox(t, db, ctx)
	require.Len(t, rows, 2)
	var withTx, withoutTx map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Payload, &withTx))
	require.NoError(t, json.Unmarshal(rows[1].Payload, &withoutTx))
	require.Equal(t, "tx_01", withTx["transaction_id"])
	require.NotContains(t, withoutTx, "transaction_id", "单文档路径 transaction_id 必须缺省")

	// 信封往返：UnmarshalEnvelope 恢复 transaction_id / seq（Stream 载荷路径）。
	ev, err := UnmarshalEnvelope(rows[0].Payload)
	require.NoError(t, err)
	require.Equal(t, "tx_01", ev.TransactionID)
	require.Positive(t, rows[0].Seq)
}

// TestOutboxWorker_EnqueueCarriesSeq：dispatch 回填 seq 到出站信封
// （Stream 条目与 WS 帧 seq 的唯一注入点）。
func TestOutboxWorker_EnqueueCarriesSeq(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rec := &recordingTransport{}
	w := setupOutboxWorker(t, rec)
	ctx := context.Background()

	ev := testEnvelope()
	ev.EventID = "seq-carry-1"
	require.NoError(t, NewEventOutbox(w.db).Publish(ctx, ev))

	require.NoError(t, w.pollOnce(ctx))
	require.Len(t, rec.enqueued, 1)
	require.Positive(t, rec.enqueued[0].Seq, "worker dispatch 必须把行 seq 回填进出站信封")
	require.Equal(t, ev.EventID, rec.enqueued[0].EventID)

	var _ *clients.Database = w.db // 保持与既有 worker 测试同款引用
}
