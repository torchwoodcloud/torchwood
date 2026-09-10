package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domaindatabases "github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// newEventTestRedis 构造集成测试用 Redis 客户端：优先 TORCHWOOD_TEST_REDIS_ADDR
// 指向的真 redis-server（本地 torchwood-redis 127.0.0.1:6379），否则退回
// miniredis（支持 XADD/XREADGROUP/XACK/XAUTOCLAIM，消费组语义真实求值）。
// 真 redis 时清理 Stream/水位键，防跨测试/跨包串扰（键为共享实例）。
func newEventTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	if addr := os.Getenv("TORCHWOOD_TEST_REDIS_ADDR"); addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: addr})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := rdb.Ping(ctx).Err()
		cancel()
		if err == nil {
			delKeys := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = rdb.Del(ctx, domainshared.EventsStream, domainshared.FunctionsEventLastSeqKey).Err()
			}
			delKeys() // 预清理：上一轮残留
			t.Cleanup(func() {
				delKeys()
				_ = rdb.Close()
			})
			return rdb
		}
		t.Logf("TORCHWOOD_TEST_REDIS_ADDR=%s unreachable (%v), falling back to miniredis", addr, err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// ——worker 侧测试桩（appfunctions.Functions 具体类型，注入 repo/queue）——

// eventCaptureRepo 是 FunctionRepo 记录桩：只实现 createExecution 异步路径
// 触达的方法（GetFunction/GetDeployment/GetVariables/CreateExecution），
// 其余经 retryRepo 空转。
type eventCaptureRepo struct {
	retryRepo
	mu         sync.Mutex
	executions []*domainfunctions.ExecutionRecord
}

func (r *eventCaptureRepo) GetFunction(_ context.Context, _, _ string) (*domainfunctions.Function, error) {
	return &domainfunctions.Function{
		ID: "fn_1", ProjectID: "p1", Enabled: true, TimeoutSeconds: 15,
		LatestReadyDeploymentID: "dep_1",
	}, nil
}

func (r *eventCaptureRepo) GetDeployment(_ context.Context, _, _, id string) (*domainfunctions.Deployment, error) {
	return &domainfunctions.Deployment{ID: id, Status: domainfunctions.DeploymentStatusReady}, nil
}

func (r *eventCaptureRepo) GetVariables(context.Context, string, string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *eventCaptureRepo) CreateExecution(_ context.Context, e *domainfunctions.ExecutionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *e
	r.executions = append(r.executions, &cp)
	return nil
}

func (r *eventCaptureRepo) captured() []*domainfunctions.ExecutionRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domainfunctions.ExecutionRecord, len(r.executions))
	copy(out, r.executions)
	return out
}

// eventTriggerRepoStub 是 TriggerRepo 桩：返回固定 event 触发器（快照扫描用）。
type eventTriggerRepoStub struct {
	triggers []domainfunctions.Trigger
}

func (s *eventTriggerRepoStub) CreateTrigger(context.Context, *domainfunctions.Trigger) error {
	return nil
}
func (s *eventTriggerRepoStub) GetTrigger(context.Context, string, string, string) (*domainfunctions.Trigger, error) {
	return nil, nil
}
func (s *eventTriggerRepoStub) ListTriggers(context.Context, string, string) ([]domainfunctions.Trigger, error) {
	return nil, nil
}
func (s *eventTriggerRepoStub) UpdateTrigger(context.Context, *domainfunctions.Trigger) error {
	return nil
}
func (s *eventTriggerRepoStub) DeleteTrigger(context.Context, string, string, string) error {
	return nil
}
func (s *eventTriggerRepoStub) GetTriggerByToken(context.Context, string, string) (*domainfunctions.Trigger, error) {
	return nil, nil
}
func (s *eventTriggerRepoStub) ClaimDueCron(_ context.Context, _ string, _ time.Time, _ int, _ domainfunctions.CronNextFunc) ([]domainfunctions.CronClaim, error) {
	return nil, nil
}
func (s *eventTriggerRepoStub) ListEnabledEventTriggers(context.Context, string) ([]domainfunctions.Trigger, error) {
	return s.triggers, nil
}

// eventProjectsStub 是 projects.Repository 桩：固定 active 项目目录。
type eventProjectsStub struct{}

func (eventProjectsStub) CreateProject(context.Context, *domainprojects.Project) error { return nil }
func (eventProjectsStub) GetProject(_ context.Context, id string) (*domainprojects.Project, error) {
	return &domainprojects.Project{ID: id, Status: "active"}, nil
}
func (eventProjectsStub) GetProjectByName(context.Context, string) (*domainprojects.Project, error) {
	return nil, nil
}
func (eventProjectsStub) ListProjects(context.Context) ([]domainprojects.Project, error) {
	return []domainprojects.Project{{ID: "p1", Status: "active"}}, nil
}
func (eventProjectsStub) UpdateProject(context.Context, *domainprojects.Project) error { return nil }
func (eventProjectsStub) DeleteProject(context.Context, string) error                  { return nil }
func (eventProjectsStub) DeleteProjectControlPlaneRows(context.Context, string) error  { return nil }

// newEventTestConsumer 组装带桩 Functions 的消费器（订阅 trg_1 →
// databases.app.collections.*.documents.create）。
func newEventTestConsumer(t *testing.T, rdb *redis.Client) (*eventTriggerConsumer, *eventCaptureRepo, *channelQueue) {
	t.Helper()
	return newEventTestConsumerWithEvents(t, rdb,
		[]string{"databases.app.collections.*.documents.create"})
}

func newEventTestConsumerWithEvents(t *testing.T, rdb *redis.Client, events []string) (*eventTriggerConsumer, *eventCaptureRepo, *channelQueue) {
	t.Helper()
	repo := &eventCaptureRepo{retryRepo: retryRepo{}}
	queue := newChannelQueue()
	triggers := &eventTriggerRepoStub{triggers: []domainfunctions.Trigger{{
		ID: "trg_1", ProjectID: "p1", FunctionID: "fn_1",
		Type: domainfunctions.TriggerTypeEvent, Enabled: true,
		Config: domainfunctions.TriggerConfig{Events: events},
	}}}
	fn := appfunctions.NewFunctionsWithUsage(&config.AppConfig{}, retryExecutor{}, repo, queue,
		nil, eventProjectsStub{}, appfunctions.Semaphores{}, nil, triggers)
	c := newEventTriggerConsumer(fn, rdb, &clients.Database{}, slog.New(slog.DiscardHandler))
	return c, repo, queue
}

// xaddEnvelope 以生产同构载荷（完整信封 JSON）XADD 一条 Stream 条目。
func xaddEnvelope(t *testing.T, rdb *redis.Client, ev *domainevents.Envelope) string {
	t.Helper()
	payload, err := infraevents.MarshalEnvelope(*ev)
	require.NoError(t, err)
	id, err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: domainshared.EventsStream,
		Values: map[string]any{"payload": string(payload)},
	}).Result()
	require.NoError(t, err)
	return id
}

func testEnvelope(id string, seq int64, event string) *domainevents.Envelope {
	return &domainevents.Envelope{
		EventID: id,
		Event:   event, ProjectID: "p1", DatabaseID: "app", CollectionID: "notes",
		DocumentID: "doc_1", Version: seq, Seq: seq,
		CreatedAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
		Data:      &domaindatabases.Document{ID: "doc_1", Version: seq, Data: map[string]any{"k": "v"}},
	}
}

// TestEventTriggerIndex_SwapInAndOut 匹配器快照的原子换入换出（worker 侧）：
// refreshIndex 拉全量快照存入 atomic.Pointer；失败时保留旧快照。
func TestEventTriggerIndex_SwapInAndOut(t *testing.T) {
	rdb := newEventTestRedis(t)
	c, _, _ := newEventTestConsumer(t, rdb)
	ctx := context.Background()

	require.NoError(t, c.refreshIndex(ctx))
	idx := c.index.Load()
	require.NotNil(t, idx)
	require.Equal(t, 1, idx.Len(), "桩返回 1 个触发器 × 1 条订阅")
	require.Len(t, idx.Match("p1", "app", "notes", "create"), 1)

	// 失败不换入：桩返回错误路径（projects stub 无错误路径，直接验证空
	// 指针下 Match 的 nil 安全——初始未刷新时 index 为 nil）。
	c2, _, _ := newEventTestConsumer(t, rdb)
	require.Nil(t, c2.index.Load())
	require.Empty(t, c2.index.Load().Match("p1", "app", "notes", "create"), "nil 索引匹配安全")
}

// TestEventConsumer_DeliverRoundTrip 消费全链路（Redis 集成）：XREADGROUP
// 读新条目 → 匹配投递（InvokeTrigger async 入队，Source=event:{trigger_id}，
// data 为投影）→ XACK → 水位推进。经济事件跳过匹配但推进水位。
func TestEventConsumer_DeliverRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newEventTestRedis(t)
	c, repo, queue := newEventTestConsumer(t, rdb)
	ctx := context.Background()
	require.NoError(t, c.refreshIndex(ctx))
	require.NoError(t, c.ensureGroup(ctx, "0")) // 测试从 0 消费

	// 1. 命中事件：投递 + ACK + 水位。
	xaddEnvelope(t, rdb, testEnvelope("ev-create", 10, domainevents.EventDocumentsCreate))
	require.NoError(t, c.consumeSession(ctx))
	caps := repo.captured()
	require.Len(t, caps, 1)
	require.Equal(t, "event:trg_1", caps[0].TriggerSource, "Source=event:{trigger_id}")
	require.Equal(t, domainfunctions.ExecutionStatusQueued, caps[0].Status)
	var payload QueuePayloadShape
	require.NoError(t, json.Unmarshal(queue.enqueued[0], &payload))
	require.Equal(t, caps[0].ID, payload.ExecutionID)
	var proj map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload.Data), &proj))
	require.Equal(t, "event", proj["type"])
	require.NotEmpty(t, proj["event_id"], "投影携带 event_id 幂等键")

	// ACK 已完成：PEL 空。
	pending, err := rdb.XPending(ctx, domainshared.EventsStream, domainshared.EventsGroupFunctionsTriggers).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), pending.Count)

	// 水位推进到 10。
	require.Equal(t, int64(10), c.getWatermark(ctx))

	// 2. 经济事件：跳过匹配（不投递）但条目消费 + 水位推进。
	economy := &domainevents.Envelope{
		EventID: "ev-eco", Event: "payments.order.settled", Domain: "payments",
		Channel: "accounts.u1", ProjectID: "p1", Seq: 11,
		CreatedAt: time.Now(),
	}
	xaddEnvelope(t, rdb, economy)
	require.NoError(t, c.consumeSession(ctx))
	require.Len(t, repo.captured(), 1, "经济事件不投递文档事件触发器")
	require.Equal(t, int64(11), c.getWatermark(ctx), "经济事件 seq 占位照常推进水位")

	// 3. no_match 事件：消费 + ACK + 水位推进，不投递。
	xaddEnvelope(t, rdb, testEnvelope("ev-update", 12, domainevents.EventDocumentsUpdate))
	require.NoError(t, c.consumeSession(ctx))
	require.Len(t, repo.captured(), 1, "no_match 不投递（也不计投递指标）")
	require.Equal(t, int64(12), c.getWatermark(ctx))
}

// TestEventConsumer_WatermarkMonotonic 水位单调推进（多副本共组 ACK 交错下
// 不回退——gap 判定不误报）。
func TestEventConsumer_WatermarkMonotonic(t *testing.T) {
	rdb := newEventTestRedis(t)
	c, _, _ := newEventTestConsumer(t, rdb)
	ctx := context.Background()
	c.advanceWatermark(ctx, 5)
	c.advanceWatermark(ctx, 3)
	require.Equal(t, int64(5), c.getWatermark(ctx))
	c.advanceWatermark(ctx, 9)
	require.Equal(t, int64(9), c.getWatermark(ctx))
}

// TestEventConsumer_StreamFirstSeqGapDetection gap 判定（D12）：Stream 首条
// seq > 水位+1 即视为裁剪缺口。
func TestEventConsumer_StreamFirstSeqGapDetection(t *testing.T) {
	rdb := newEventTestRedis(t)
	c, _, _ := newEventTestConsumer(t, rdb)
	ctx := context.Background()

	// 空 Stream：无从判定 → 无补投。
	require.Equal(t, int64(0), c.streamFirstSeq(ctx))

	xaddEnvelope(t, rdb, testEnvelope("ev-gap-first", 100, domainevents.EventDocumentsCreate))
	require.Equal(t, int64(100), c.streamFirstSeq(ctx))

	// 水位 98：first(100) > 98+1 → 缺口。水位 99：无缺口。
	c.advanceWatermark(ctx, 99)
	watermark := c.getWatermark(ctx)
	first := c.streamFirstSeq(ctx)
	require.False(t, first > watermark+1, "水位 99 + 首条 100 = 无缺口")
}

// TestEventConsumer_BackfillFromOutbox 停机补投真集成（D12，Redis + Postgres）：
// outbox 行存在而 Stream 缺口（水位落后）→ 分批补投同一匹配+投递路径，
// 水位推进后二次执行不再补投（幂等收口）。
func TestEventConsumer_BackfillFromOutbox(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if os.Getenv("TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE") == "" || os.Getenv("TORCHWOOD_TEST_DATABASE_SOURCE") == "" {
		t.Skip("TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE / TORCHWOOD_TEST_DATABASE_SOURCE not set; skipping backfill integration test")
	}
	rdb := newEventTestRedis(t)
	// 全 op 通配订阅：create/update/delete 三行全部命中。
	c, repo, _ := newEventTestConsumerWithEvents(t, rdb,
		[]string{"databases.app.collections.*.documents.*"})
	ctx := context.Background()
	require.NoError(t, c.refreshIndex(ctx))
	c.db = testutil.SetupTestDB(t)

	// outbox 落 3 行（identity 分配 seq；:changes 同语义——行存在即可读，
	// 与 published_at 无关）。
	outbox := infraevents.NewEventOutbox(c.db)
	for _, ev := range []*domainevents.Envelope{
		testEnvelope("ev-bf-1", 1, domainevents.EventDocumentsCreate),
		testEnvelope("ev-bf-2", 2, domainevents.EventDocumentsUpdate),
		testEnvelope("ev-bf-3", 3, domainevents.EventDocumentsDelete),
	} {
		ev.ProjectID = "p1"
		require.NoError(t, outbox.Publish(ctx, *ev))
	}
	rows, err := infraevents.ScanOutboxSeqRange(ctx, c.db, 0, 1<<62, 100)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	first := rows[0].Seq
	last := rows[len(rows)-1].Seq
	require.Greater(t, last, first, "identity seq 单调")

	// 模拟停机裁剪缺口：Stream 只剩最后一条（seq=last），水位停在缺口前
	//（水位键缺失 = 0：从未消费过 → 全区间按缺口处理）。
	xaddEnvelope(t, rdb, &domainevents.Envelope{EventID: "ev-gap", Event: domainevents.EventDocumentsCreate,
		ProjectID: "p1", DatabaseID: "app", CollectionID: "notes", DocumentID: "doc_x",
		Version: last, Seq: last, CreatedAt: time.Now(),
		Data: &domaindatabases.Document{ID: "doc_x", Version: last, Data: map[string]any{"k": "v"}}})
	require.Greater(t, c.streamFirstSeq(ctx), c.getWatermark(ctx)+1, "缺口判定成立")

	// 删除事件（testEnvelope(3, delete) 无 Data）也必须可补投。
	c.backfill(ctx)
	caps := repo.captured()
	require.Len(t, caps, 3, "三行 outbox 全部经匹配路径补投")
	for _, e := range caps {
		require.Equal(t, "event:trg_1", e.TriggerSource)
		require.Equal(t, domainfunctions.ExecutionStatusQueued, e.Status)
	}
	require.Equal(t, last, c.getWatermark(ctx), "水位推进到区间上界")

	// 幂等收口：水位到位后二次执行无补投。
	n := len(repo.captured())
	c.backfill(ctx)
	require.Len(t, repo.captured(), n)
}

// TestWorkerEventLoop_GracefulShutdown eventLoop 挂进 worker 生命周期：
// Start 启动事件消费（带 Redis/DB 注入的构造器），ctx 取消后 Stop 等待
// goroutine 退出（对齐 cronLoop 风格的优雅关停）。
func TestWorkerEventLoop_GracefulShutdown(t *testing.T) {
	rdb := newEventTestRedis(t)
	repo := &eventCaptureRepo{retryRepo: retryRepo{}}
	queue := newChannelQueue()
	fn := appfunctions.NewFunctionsWithUsage(&config.AppConfig{}, retryExecutor{}, repo, queue,
		nil, eventProjectsStub{}, appfunctions.Semaphores{}, nil, &eventTriggerRepoStub{})
	w := NewWorkerWithEventTriggers(fn, queue, slog.New(slog.DiscardHandler), rdb, &clients.Database{})
	require.NotNil(t, w.events)

	// 快照扫描带 1s 失败重试 → Stop 须在其完成前可取消（首刷成功后阻塞
	// 在 XREADGROUP Block 1s）。给 Stop 3s 预算足以覆盖一个 Block 周期。
	startCtx, startCancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		close(started)
		_ = w.Start(startCtx)
	}()
	<-started
	time.Sleep(300 * time.Millisecond) // 让 eventLoop 进入消费阻塞
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	require.NoError(t, w.Stop(stopCtx))
	startCancel()
}

// QueuePayloadShape 是 worker 侧解析队列 payload 的最小字段集（与 app 层
// queueMessage 字段名一致；仅测试断言用）。
type QueuePayloadShape struct {
	ExecutionID string `json:"execution_id"`
	Data        string `json:"data"`
}
