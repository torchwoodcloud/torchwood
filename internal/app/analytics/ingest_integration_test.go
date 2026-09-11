// 外部测试包（analytics_test）：摄入链端到端集成——装配生产同构的
// clientgrpc/servergrpc handler（它们 import 本包，内部测试包会成环）。
package analytics_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/internal/api/clientgrpc"
	"github.com/torchwoodcloud/torchwood/internal/api/servergrpc"
	"github.com/torchwoodcloud/torchwood/internal/app/analytics"
	appclient "github.com/torchwoodcloud/torchwood/internal/app/client"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	infrabilling "github.com/torchwoodcloud/torchwood/internal/infra/billing"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ingestTestEnv 聚合摄入链集成测试的生产同构装配（真实 Postgres 项目 schema
// analytics_* 表 + miniredis 上的真 RedisCounter 计量 + testutil.InterceptorEnv
// 生产同构拦截器链 clientInfo → auth → rate limit → audit）。
type ingestTestEnv struct {
	ctx       context.Context
	db        *clients.Database
	projectID string
	schema    string // 已 quote 的项目 schema 名
	ingest    *analytics.Ingest
	counter   *infrabilling.RedisCounter
	env       *testutil.InterceptorEnv
	sessions  *auth.SessionService
}

func setupIngestEnv(t *testing.T) *ingestTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := infrabilling.NewRedisCounter(rdb)

	ingest := analytics.NewIngest(bunrepo.NewAnalyticsIngestRepository(db), counter, nil)

	cfg := &config.AppConfig{Security: &config.Security{Jwt: &config.Security_Jwt{Secret: "analytics-ingest-test-secret"}}} // #nosec G101 -- 测试固定密钥
	env, err := testutil.NewInterceptorEnv(db, cfg, nil)
	require.NoError(t, err)

	usersRepo := bunrepo.NewUserRepository(db)
	sessionRepo := bunrepo.NewSessionRepository(db)
	roles := appclient.NewUserRoles(usersRepo, bunrepo.NewMembershipRepository(db))
	sessions := auth.NewSessionService(cfg, sessionRepo, roles, auth.NewRedisRefreshRotationStore(rdb))

	return &ingestTestEnv{
		ctx:       ctx,
		db:        db,
		projectID: projectID,
		schema:    testutil.CatalogQuoted(projectID),
		ingest:    ingest,
		counter:   counter,
		env:       env,
		sessions:  sessions,
	}
}

// createAnonymousSession 镜像 Account.CreateAnonymousSession 的核心路径：
// 注册匿名用户（anonymous label，users 表真实行）→ 真会话行 + 真签发
// access token（与 InterceptorEnv validator 共享 jwt secret，Bearer 全链路可验）。
func (e *ingestTestEnv) createAnonymousSession(t *testing.T) (userID, accessToken string) {
	t.Helper()
	userID = idgen.ULID().String()
	registered, err := users.Register(users.RegisterInput{
		ID:        userID,
		Email:     users.AnonymousEmail(userID),
		Name:      "Anonymous",
		Anonymous: true,
	})
	require.NoError(t, err)
	require.NoError(t, bunrepo.NewUserRepository(e.db).Insert(e.ctx, e.projectID, registered))
	bundle, _, err := e.sessions.CreateSessionAndTokens(e.ctx, e.projectID, userID, registered.Email, "anonymous")
	require.NoError(t, err)
	require.NotEmpty(t, bundle.AccessToken)
	return userID, bundle.AccessToken
}

func (e *ingestTestEnv) countEvents(t *testing.T, where string, args ...any) int {
	t.Helper()
	q := fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events`, e.schema)
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	require.NoError(t, e.db.QueryRowContext(e.ctx, q, args...).Scan(&n))
	return n
}

// defRow 读字典行 first_seen/last_seen。
func (e *ingestTestEnv) defRow(t *testing.T, name string) (first, last time.Time) {
	t.Helper()
	require.NoError(t, e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT first_seen, last_seen FROM %s.analytics_event_definitions WHERE name = ?`, e.schema), name).
		Scan(&first, &last))
	return first, last
}

// meterValue 读当前小时计量桶（Redis 小时桶 = accepted 断言）。
func (e *ingestTestEnv) meterValue(t *testing.T) int64 {
	t.Helper()
	v, err := e.counter.Get(e.ctx, e.projectID, domainbilling.MetricAnalyticsEvents, domainbilling.HourBucket(time.Now()))
	require.NoError(t, err)
	return v
}

// runClient 面调用：生产同构拦截器链 + 真 clientgrpc handler（链路透传的
// req 恒 nil——InvokeUnaryHandler 只验证链路语义，业务请求经闭包捕获）。
func (e *ingestTestEnv) runClient(t *testing.T, md metadata.MD, req *clientv1.IngestEventsRequest) (*clientv1.IngestEventsResponse, error) {
	t.Helper()
	handler := clientgrpc.NewAnalyticsService(e.ingest)
	var resp *clientv1.IngestEventsResponse
	err := e.env.InvokeUnaryHandler(e.ctx, testutil.MethodAnalyticsClientIngest, md,
		func(ctx context.Context, _ any) (any, error) {
			var inErr error
			resp, inErr = handler.IngestEvents(ctx, req)
			return resp, inErr
		})
	return resp, err
}

// runServer 面调用：生产同构拦截器链 + 真 servergrpc handler。
func (e *ingestTestEnv) runServer(t *testing.T, md metadata.MD, req *serverv1.IngestServerEventsRequest) (*serverv1.IngestEventsResponse, error) {
	t.Helper()
	handler := servergrpc.NewAnalyticsService(e.ingest)
	var resp *serverv1.IngestEventsResponse
	err := e.env.InvokeUnaryHandler(e.ctx, testutil.MethodAnalyticsServerIngest, md,
		func(ctx context.Context, _ any) (any, error) {
			var inErr error
			resp, inErr = handler.IngestEvents(ctx, req)
			return resp, inErr
		})
	return resp, err
}

// TestIngestIntegration_ClientFaceEndToEnd（PR2 验收）：
//   - 匿名会话经 Bearer → principal 全链路摄入成功（归因=匿名用户、source=client）；
//   - 无凭证调用被认证拦截器拒绝（Unauthenticated——门禁=至少匿名会话）；
//   - 混合批（越界时间戳/坏 props）部分接收：accepted+skipped=批大小、
//     落库行数=accepted；
//   - 计量：Redis 小时桶 = accepted。
func TestIngestIntegration_ClientFaceEndToEnd(t *testing.T) {
	e := setupIngestEnv(t)
	userID, token := e.createAnonymousSession(t)
	md := metadata.Pairs("authorization", "Bearer "+token)

	// 无凭证：认证拦截器拒绝（不达 handler）。
	_, err := e.runClient(t, metadata.MD{}, &clientv1.IngestEventsRequest{Events: []*clientv1.AnalyticsEvent{{Name: "e"}}})
	require.Equal(t, codes.Unauthenticated, status.Code(err), "无会话必须被拒（门禁=至少匿名会话）")
	require.Equal(t, 0, e.countEvents(t, ""))

	// 混合批：2 好 + 3 坏（越界 past / 越界 future / object props）。
	now := time.Now().UTC()
	resp, err := e.runClient(t, md, &clientv1.IngestEventsRequest{Events: []*clientv1.AnalyticsEvent{
		{Name: "level_complete", SessionId: "game-session-1", Props: map[string]*structpb.Value{
			"level": structpb.NewNumberValue(3),
		}},
		{Name: "ad_watch", OccurredAt: timestamppb.New(now.Add(-time.Hour))},
		{Name: "too_old", OccurredAt: timestamppb.New(now.Add(-25 * time.Hour))},
		{Name: "too_new", OccurredAt: timestamppb.New(now.Add(10 * time.Minute))},
		{Name: "bad_props", Props: map[string]*structpb.Value{
			"obj": structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{}}),
		}},
	}})
	require.NoError(t, err)
	require.Equal(t, int32(2), resp.GetAccepted())
	require.Equal(t, int32(3), resp.GetSkipped())
	require.Equal(t, 5, int(resp.GetAccepted()+resp.GetSkipped()), "accepted+skipped 必须等于批大小")

	// 落库行数 = accepted；归因/source 服务端落定。
	require.Equal(t, 2, e.countEvents(t, ""))
	require.Equal(t, 2, e.countEvents(t, "user_id = ? AND source = 'client'", userID))
	require.Equal(t, 1, e.countEvents(t, "name = ? AND session_id = 'game-session-1'", "level_complete"))
	// 缺省 occurred_at 落服务端 now（窗内）；显式过去时间保真。
	require.Equal(t, 1, e.countEvents(t, "name = 'ad_watch' AND occurred_at < now() - interval '30 minutes'"))

	// 计量桶 = accepted。
	require.Equal(t, int64(2), e.meterValue(t))

	// 字典：两个新名 first/last_seen 落库。
	for _, name := range []string{"level_complete", "ad_watch"} {
		first, last := e.defRow(t, name)
		require.False(t, first.IsZero(), "%s first_seen 应落库", name)
		require.False(t, last.IsZero())
	}
}

// TestIngestIntegration_ServerFaceTrustedUserAndAudit（PR2 验收）：
//   - API key（analytics:write scope）可信代报 user_id 落库、source=server；
//   - 摄入后 audit_logs 无新行（D13 护栏，真审计仓储断言）；
//   - 计量桶 = accepted。
func TestIngestIntegration_ServerFaceTrustedUserAndAudit(t *testing.T) {
	e := setupIngestEnv(t)
	secret, dropKey := testutil.CreateTestAPIKey(e.ctx, e.db, e.projectID, []string{"analytics.write"})
	t.Cleanup(dropKey)

	auditBefore, err := e.env.AuditLogCount(e.ctx)
	require.NoError(t, err)

	resp, err := e.runServer(t, metadata.Pairs("x-api-key", secret), &serverv1.IngestServerEventsRequest{Events: []*serverv1.ServerAnalyticsEvent{
		{Name: "payment_done", UserId: strPtr("user-77"), SessionId: "srv-ctx"},
		{Name: "cron_tick"}, // 无归属
		{Name: "bad", OccurredAt: timestamppb.New(time.Now().Add(-48 * time.Hour))}, // skipped
	}})
	require.NoError(t, err)
	require.Equal(t, int32(2), resp.GetAccepted())
	require.Equal(t, int32(1), resp.GetSkipped())

	require.Equal(t, 1, e.countEvents(t, "name = 'payment_done' AND user_id = 'user-77' AND source = 'server' AND session_id = 'srv-ctx'"))
	require.Equal(t, 1, e.countEvents(t, "name = 'cron_tick' AND user_id = '' AND source = 'server'"))
	require.Equal(t, int64(2), e.meterValue(t))

	// D13 红线：摄入零审计行（成功路径 + 显式静默登记）。
	auditAfter, err := e.env.AuditLogCount(e.ctx)
	require.NoError(t, err)
	require.Equal(t, auditBefore, auditAfter, "server 面 IngestEvents 不得落 audit_logs 行（D13）")
}

// TestIngestIntegration_DictionarySoftCap（PR2 验收）：预置 1000 字典名后，
// 第 1001 个新名 skip、存量名照常；字典 upsert 的 first_seen 不回退、
// last_seen 推进。
func TestIngestIntegration_DictionarySoftCap(t *testing.T) {
	e := setupIngestEnv(t)

	// 种 MaxEventNames 个字典名（单条多 VALUES 直插，含存量名 seeded_name）。
	values := ""
	args := make([]any, 0, domainanalytics.MaxEventNames*3)
	seed := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < domainanalytics.MaxEventNames; i++ {
		name := fmt.Sprintf("seeded_%04d", i)
		if i == 0 {
			name = "seeded_name"
		}
		if values != "" {
			values += ","
		}
		values += "(?, ?, ?)"
		args = append(args, name, seed, seed)
	}
	_, err := e.db.ExecContext(e.ctx,
		fmt.Sprintf(`INSERT INTO %s.analytics_event_definitions (name, first_seen, last_seen) VALUES %s`, e.schema, values), args...)
	require.NoError(t, err)

	secret, dropKey := testutil.CreateTestAPIKey(e.ctx, e.db, e.projectID, []string{"analytics"})
	t.Cleanup(dropKey)
	resp, err := e.runServer(t, metadata.Pairs("x-api-key", secret), &serverv1.IngestServerEventsRequest{Events: []*serverv1.ServerAnalyticsEvent{
		{Name: "seeded_name"},    // 存量名：照收
		{Name: "brand_new_1001"}, // 第 1001 个新名：skip
	}})
	require.NoError(t, err)
	require.Equal(t, int32(1), resp.GetAccepted())
	require.Equal(t, int32(1), resp.GetSkipped())
	require.Equal(t, 1, e.countEvents(t, "name = 'seeded_name'"))
	require.Equal(t, 0, e.countEvents(t, "name = 'brand_new_1001'"))
	require.Equal(t, 0, e.countEvents(t, "name = ?", "brand_new_1001"))

	// 字典语义：存量名 first_seen 不回退（保持 seed，微秒精度对齐 PG
	// timestamptz）、last_seen 推进到本次。
	first, last := e.defRow(t, "seeded_name")
	require.Equal(t, seed.Truncate(time.Microsecond), first.UTC(), "first_seen 不得回退")
	require.True(t, last.After(seed), "last_seen 应推进")

	// 软上限下新名不入字典。
	var n int
	require.NoError(t, e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.analytics_event_definitions WHERE name = 'brand_new_1001'`, e.schema)).Scan(&n))
	require.Equal(t, 0, n)
}

func strPtr(s string) *string { return &s }
