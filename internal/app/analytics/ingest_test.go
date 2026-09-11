package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeIngestRepo 是 IngestRepository 的内存桩：记录写入调用，可注入故障与
// 字典预置状态（软上限行为测试）。
type fakeIngestRepo struct {
	inserted   []domainanalytics.Event
	upserted   []domainanalytics.EventDefinition
	defs       map[string]domainanalytics.EventDefinition // 预置字典
	insertErr  error
	upsertErr  error
	listErr    error
	countCalls int
	listCalls  int
}

func (r *fakeIngestRepo) InsertEvents(_ context.Context, _ string, events []domainanalytics.Event) error {
	if r.insertErr != nil {
		return r.insertErr
	}
	r.inserted = append(r.inserted, events...)
	return nil
}

func (r *fakeIngestRepo) UpsertEventDefinitions(_ context.Context, _ string, defs []domainanalytics.EventDefinition) error {
	if r.upsertErr != nil {
		return r.upsertErr
	}
	if r.defs == nil {
		r.defs = map[string]domainanalytics.EventDefinition{}
	}
	for _, d := range defs {
		if old, ok := r.defs[d.Name]; ok {
			if d.LastSeen.After(old.LastSeen) {
				d.FirstSeen = old.FirstSeen // first_seen 不回退
				r.defs[d.Name] = d
			}
			continue
		}
		r.defs[d.Name] = d
	}
	r.upserted = append(r.upserted, defs...)
	return nil
}

func (r *fakeIngestRepo) CountEventDefinitions(context.Context, string) (int64, error) {
	r.countCalls++
	if r.defs == nil {
		return 0, nil
	}
	return int64(len(r.defs)), nil
}

func (r *fakeIngestRepo) ListEventDefinitionNames(context.Context, string) ([]string, error) {
	r.listCalls++
	if r.listErr != nil {
		return nil, r.listErr
	}
	names := make([]string, 0, len(r.defs))
	for n := range r.defs {
		names = append(names, n)
	}
	return names, nil
}

// fakeUsageCounter 记录 Incr 调用（计量断言）。
type fakeUsageCounter struct {
	incr map[string]int64
	err  error
}

func (c *fakeUsageCounter) Incr(_ context.Context, projectID, metric string, delta int64) error {
	if c.err != nil {
		return c.err
	}
	if c.incr == nil {
		c.incr = map[string]int64{}
	}
	c.incr[projectID+"/"+metric] += delta
	return nil
}

func (c *fakeUsageCounter) IncrAt(context.Context, string, string, time.Time, int64) error {
	return nil
}
func (c *fakeUsageCounter) Set(context.Context, string, string, time.Time, int64) error { return nil }
func (c *fakeUsageCounter) Get(context.Context, string, string, time.Time) (int64, error) {
	return 0, nil
}
func (c *fakeUsageCounter) ListHour(context.Context, time.Time) ([]domainbilling.Bucket, error) {
	return nil, nil
}

var testNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func newTestIngest(repo *fakeIngestRepo, usage *fakeUsageCounter) *Ingest {
	return &Ingest{repo: repo, usage: usage, logger: slog.Default(), now: func() time.Time { return testNow }}
}

func mustValue(t *testing.T, v any) *structpb.Value {
	t.Helper()
	sv, err := structpb.NewValue(v)
	require.NoError(t, err)
	return sv
}

func tsPtr(t time.Time) *time.Time { return &t }

// TestIngestEvents_MixedBatchPartialAcceptance（PR2 验收）：越界时间戳/坏
// props 的事件 skipped、好事件落库，accepted+skipped = 批大小，不拒整批。
func TestIngestEvents_MixedBatchPartialAcceptance(t *testing.T) {
	repo := &fakeIngestRepo{}
	usage := &fakeUsageCounter{}
	u := newTestIngest(repo, usage)

	res, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID:    "p1",
		Source:       domainanalytics.SourceClient,
		ClientUserID: "user-1",
		Events: []IncomingEvent{
			{Name: "level_complete", Props: map[string]*structpb.Value{"level": mustValue(t, 3.0)}},
			{Name: "level_complete", OccurredAt: tsPtr(testNow.Add(-25 * time.Hour))}, // 越界 past
			{Name: "level_complete", OccurredAt: tsPtr(testNow.Add(6 * time.Minute))}, // 越界 future
			{Name: "bad_nested", Props: map[string]*structpb.Value{ // object 值 → 拒
				"obj": structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{"x": structpb.NewStringValue("y")}})}},
			{Name: "bad_key", Props: map[string]*structpb.Value{"1bad": mustValue(t, "v")}}, // 键正则 → 拒
			{Name: "1badname"}, // 事件名正则 → 拒（防御性复检）
			{Name: "ad_watch", OccurredAt: tsPtr(testNow.Add(-time.Hour))}, // 窗内补传 → 收
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), res.Accepted)
	require.Equal(t, int32(5), res.Skipped)
	require.Len(t, repo.inserted, 2)

	// 落库形态：归因/时间/source 服务端落定。
	for _, e := range repo.inserted {
		require.Equal(t, "user-1", e.UserID, "client 面 user_id 必须来自 Principal")
		require.Equal(t, domainanalytics.SourceClient, e.Source)
		require.Equal(t, testNow, e.IngestedAt)
	}
	require.Equal(t, testNow, repo.inserted[0].OccurredAt, "缺省 occurred_at = 服务端 now")
	require.Equal(t, testNow.Add(-time.Hour), repo.inserted[1].OccurredAt)

	// 字典：好事件名 upsert（含 first/last_seen = 批级 now）。
	require.ElementsMatch(t, []string{"level_complete", "ad_watch"}, defNames(repo.upserted))
}

// TestIngestEvents_ClampWindowBounds（D4 边界）：now-24h 与 now+5min 恰在窗
// 内（闭区间），各越 1 秒即 skipped。
func TestIngestEvents_ClampWindowBounds(t *testing.T) {
	repo := &fakeIngestRepo{}
	u := newTestIngest(repo, &fakeUsageCounter{})
	res, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{
			{Name: "at_past_edge", OccurredAt: tsPtr(testNow.Add(domainanalytics.ClampPast))},
			{Name: "at_future_edge", OccurredAt: tsPtr(testNow.Add(domainanalytics.ClampFuture))},
			{Name: "past_by_1s", OccurredAt: tsPtr(testNow.Add(domainanalytics.ClampPast - time.Second))},
			{Name: "future_by_1s", OccurredAt: tsPtr(testNow.Add(domainanalytics.ClampFuture + time.Second))},
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), res.Accepted)
	require.Equal(t, int32(2), res.Skipped)
}

// TestIngestEvents_ServerFaceTrustedUserID（D3）：server 面 per-event user_id
// 可信代报落库，缺省=无归属（空串）。
func TestIngestEvents_ServerFaceTrustedUserID(t *testing.T) {
	repo := &fakeIngestRepo{}
	u := newTestIngest(repo, &fakeUsageCounter{})
	res, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1",
		Source:    domainanalytics.SourceServer,
		Events: []IncomingEvent{
			{Name: "payment_done", UserID: "user-9", SessionID: "srv-sess"},
			{Name: "cron_tick"}, // 无归属
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), res.Accepted)
	require.Equal(t, "user-9", repo.inserted[0].UserID)
	require.Equal(t, "srv-sess", repo.inserted[0].SessionID)
	require.Equal(t, domainanalytics.SourceServer, repo.inserted[0].Source)
	require.Equal(t, "", repo.inserted[1].UserID)
}

// TestIngestEvents_ClientFaceRequiresPrincipal（红线 D3）：client 面无
// Principal 归因即拒绝（Unauthenticated），per-event UserID 不可伪造。
func TestIngestEvents_ClientFaceRequiresPrincipal(t *testing.T) {
	u := newTestIngest(&fakeIngestRepo{}, &fakeUsageCounter{})
	_, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1",
		Source:    domainanalytics.SourceClient,
		Events:    []IncomingEvent{{Name: "e", UserID: "forged"}},
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// TestIngestEvents_ClientFaceIgnoresEventUserID：client 面即使事件体带
// UserID（非 gRPC 链路误用）也恒以 Principal 归因为准。
func TestIngestEvents_ClientFaceIgnoresEventUserID(t *testing.T) {
	repo := &fakeIngestRepo{}
	u := newTestIngest(repo, &fakeUsageCounter{})
	res, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceClient, ClientUserID: "real-user",
		Events: []IncomingEvent{{Name: "e", UserID: "forged"}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Accepted)
	require.Equal(t, "real-user", repo.inserted[0].UserID)
}

// TestIngestEvents_SoftCap（D12 验收）：字典未达上限全放行；已达上限时
// 批内新名 skip、存量名照常（CountEventDefinitions >= 1000 触发存量名集合
// 拉取）。
func TestIngestEvents_SoftCap(t *testing.T) {
	// ① 未达上限：新名放行（批内可轻微超扣，可容忍）。
	repo := &fakeIngestRepo{defs: map[string]domainanalytics.EventDefinition{
		"known": {Name: "known"},
	}}
	u := newTestIngest(repo, &fakeUsageCounter{})
	res, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{{Name: "brand_new"}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Accepted)
	require.Equal(t, 0, repo.listCalls, "未达上限不得拉存量名清单")

	// ② 已达上限：存量名照收、新名 skip。
	full := map[string]domainanalytics.EventDefinition{"known": {Name: "known"}}
	for i := 0; i < domainanalytics.MaxEventNames-1; i++ {
		n := "ev" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		full[n] = domainanalytics.EventDefinition{Name: n}
	}
	require.Len(t, full, domainanalytics.MaxEventNames)
	repo = &fakeIngestRepo{defs: full}
	u = newTestIngest(repo, &fakeUsageCounter{})
	res, err = u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{
			{Name: "known"},
			{Name: "the_1001st"},
			{Name: "the_1001st"}, // 同批同名新名：两事件都 skip（字典不记得批内新名）
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Accepted)
	require.Equal(t, int32(2), res.Skipped)
	require.Equal(t, 1, repo.listCalls)
	require.Len(t, repo.inserted, 1)
	require.Equal(t, "known", repo.inserted[0].Name)
}

// TestIngestEvents_PropsNormalization：标量化（浅数组合法、嵌套拒绝）、
// 字符串截断 256、键数上限 25、16KiB 体积护栏、NaN 拒收。
func TestIngestEvents_PropsNormalization(t *testing.T) {
	long := make([]rune, 1000)
	for i := range long {
		long[i] = 'x'
	}
	props := map[string]*structpb.Value{
		"str":     mustValue(t, string(long)),               // 截断
		"arr":     mustValue(t, []any{"a", 1.0, true, nil}), // 浅数组
		"num":     mustValue(t, 3.14),
		"b":       mustValue(t, true),
		"n":       mustValue(t, nil),
		"arr_str": mustValue(t, []any{string(long)}), // 数组内字符串同样截断
	}
	repo := &fakeIngestRepo{}
	u := newTestIngest(repo, &fakeUsageCounter{})
	res, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{{Name: "props_ok", Props: props}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Accepted)

	var stored map[string]any
	require.NoError(t, json.Unmarshal(repo.inserted[0].Props, &stored))
	require.Equal(t, string(long[:domainanalytics.PropValueMaxLen]), stored["str"])
	arr := stored["arr_str"].([]any)
	require.Equal(t, string(long[:domainanalytics.PropValueMaxLen]), arr[0])
	require.Equal(t, 3.14, stored["num"])
	require.Nil(t, stored["n"])
	// 规范 JSON：键序稳定。
	require.JSONEq(t, string(repo.inserted[0].Props), string(repo.inserted[0].Props))

	// 嵌套数组（数组含 object）→ skip；NaN（protojson 可携带的非法 JSON
	// 数）→ skip，不得炸整批。
	nested := mustValue(t, []any{map[string]any{"x": "y"}})
	res, err = u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{
			{Name: "nested", Props: map[string]*structpb.Value{"a": nested}},
			{Name: "nan", Props: map[string]*structpb.Value{"n": structpb.NewNumberValue(math.NaN())}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), res.Accepted)
	require.Equal(t, int32(2), res.Skipped)

	// 键数上限：26 键 → skip（25 是上限）。
	over := map[string]*structpb.Value{}
	for i := 0; i < domainanalytics.MaxPropsKeys+1; i++ {
		over["k"+string(rune('a'+i))] = mustValue(t, 1.0)
	}
	res, err = u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{{Name: "too_many_keys", Props: over}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Skipped)

	// 16KiB 体积：25 个近满长字符串键 → 序列化超限 skip。
	big := map[string]*structpb.Value{}
	for i := 0; i < domainanalytics.MaxPropsKeys; i++ {
		big["k"+string(rune('a'+i))] = mustValue(t, string(make([]rune, 700)))
	}
	res, err = u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{{Name: "too_big", Props: big}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Skipped)
}

// TestIngestEvents_Metering：accepted 条数计量（项目+metric 维度），skipped
// 不计；全 skip 批不计量。
func TestIngestEvents_Metering(t *testing.T) {
	usage := &fakeUsageCounter{}
	u := newTestIngest(&fakeIngestRepo{}, usage)
	res, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{
			{Name: "a"}, {Name: "b"}, {Name: "c"},
			{Name: "out", OccurredAt: tsPtr(testNow.Add(-48 * time.Hour))}, // skipped
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(3), res.Accepted)
	require.Equal(t, int64(3), usage.incr["p1/"+domainbilling.MetricAnalyticsEvents])

	// 计量故障不影响摄入（best-effort）。
	failing := &fakeUsageCounter{err: errors.New("redis down")}
	u2 := newTestIngest(&fakeIngestRepo{}, failing)
	res, err = u2.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{{Name: "a"}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Accepted)
}

// TestIngestEvents_DefinitionUpsertFailureNonFatal：字典 upsert 失败不回滚
// 已落库事件（字典是发现元数据，事件是业务数据）。
func TestIngestEvents_DefinitionUpsertFailureNonFatal(t *testing.T) {
	repo := &fakeIngestRepo{upsertErr: errors.New("dict down")}
	u := newTestIngest(repo, &fakeUsageCounter{})
	res, err := u.IngestEvents(context.Background(), IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer,
		Events: []IncomingEvent{{Name: "a"}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Accepted)
	require.Len(t, repo.inserted, 1)
}

// TestIngestEvents_BatchLevelErrors：批级错误（空批/超批/缺项目/坏 source/
// 存储故障）整批拒绝。
func TestIngestEvents_BatchLevelErrors(t *testing.T) {
	u := newTestIngest(&fakeIngestRepo{}, &fakeUsageCounter{})
	ctx := context.Background()

	_, err := u.IngestEvents(ctx, IngestEventsCommand{ProjectID: "p1", Source: domainanalytics.SourceServer})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	tooMany := make([]IncomingEvent, domainanalytics.MaxBatchEvents+1)
	for i := range tooMany {
		tooMany[i] = IncomingEvent{Name: "e"}
	}
	_, err = u.IngestEvents(ctx, IngestEventsCommand{ProjectID: "p1", Source: domainanalytics.SourceServer, Events: tooMany})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = u.IngestEvents(ctx, IngestEventsCommand{Source: domainanalytics.SourceServer, Events: []IncomingEvent{{Name: "e"}}})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	_, err = u.IngestEvents(ctx, IngestEventsCommand{ProjectID: "p1", Source: "weird", Events: []IncomingEvent{{Name: "e"}}})
	require.Equal(t, codes.Internal, status.Code(err))

	failing := newTestIngest(&fakeIngestRepo{insertErr: errors.New("db down")}, &fakeUsageCounter{})
	_, err = failing.IngestEvents(ctx, IngestEventsCommand{
		ProjectID: "p1", Source: domainanalytics.SourceServer, Events: []IncomingEvent{{Name: "e"}},
	})
	require.EqualError(t, err, "db down")
}

func defNames(defs []domainanalytics.EventDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}
