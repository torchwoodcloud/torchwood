package functions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// endUserCtx 返回携带端用户 principal 的上下文（P2 客户端调用面身份门）。
func endUserCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:   "user-1",
		ActorKind: shared.ActorKindEndUser,
		UserID:    "user-1",
		ProjectID: "p1",
	})
}

// seedClientCallableFunction 构造开启客户端调用的函数。
func seedClientCallableFunction(t *testing.T, repo *mockRepo, projectID, functionID string) *domainfunctions.Function {
	t.Helper()
	fn := seedReadyFunction(repo, projectID, functionID, true, 15)
	fn.ClientCallable = true
	fn.ClientPerUserLimit = 5
	fn.ClientLimitWindow = domainfunctions.ClientLimitWindowDay
	require.NoError(t, repo.UpdateFunction(context.Background(), fn))
	return fn
}

// fakeQuotaLimiter 记录 Allow 调用并按脚本返回（限频端口 fake）。
type fakeQuotaLimiter struct {
	mu       sync.Mutex
	keys     []string
	limits   []int
	ends     []time.Time
	allowErr error // 非空 = 模拟 Redis 故障
	// counts 由测试动态给出（每次 Allow 弹出一个；空则恒放行）。
	counts []int64
	calls  int
}

func (l *fakeQuotaLimiter) Allow(_ context.Context, key string, limit int, windowEnd time.Time) (domainfunctions.QuotaResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	l.keys = append(l.keys, key)
	l.limits = append(l.limits, limit)
	l.ends = append(l.ends, windowEnd)
	if l.allowErr != nil {
		return domainfunctions.QuotaResult{}, l.allowErr
	}
	if len(l.counts) > 0 {
		c := l.counts[0]
		l.counts = l.counts[1:]
		return domainfunctions.QuotaResult{Allowed: c < int64(limit), WindowEnd: windowEnd}, nil
	}
	return domainfunctions.QuotaResult{Allowed: true, WindowEnd: windowEnd}, nil
}

// TestClientInvoke_RequireEndUser：admin / 匿名 / API key 一律拒绝；
// 端用户放行（策略门后的业务校验可证明守卫已过）。
func TestClientInvoke_RequireEndUser(t *testing.T) {
	repo := newMockRepo()
	seedClientCallableFunction(t, repo, "p1", "fn_1")
	uc := newTestUC(newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil), repo, newMockQueue())

	// admin → PermissionDenied。
	_, err := uc.ClientInvoke(platformAdminCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// 匿名（含 client_anonymous_allowed=true 的函数——字段一期禁用，管理面
	// 拒绝设置；即便存量被改 true，匿名也进不来）→ Unauthenticated。
	fn := repo.functions["fn_1"]
	fn.ClientAnonymousAllowed = true
	_, err = uc.ClientInvoke(context.Background(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// TestClientInvoke_PolicyGates：非 client_callable → PermissionDenied
// （存量全 FALSE fail-closed）；disabled → FailedPrecondition（沿用既有语义）；
// 不存在 → NotFound。
func TestClientInvoke_PolicyGates(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_server", true, 15) // 未开 client_callable
	seedClientCallableFunction(t, repo, "p1", "fn_ok")
	repo.functions["fn_ok"].Enabled = false
	uc := newTestUC(newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil), repo, newMockQueue())

	_, err := uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_server"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	_, err = uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_ok"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	_, err = uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestClientInvoke_QuotaWindows：三档窗口的 bucket 键与窗口结束时刻正确；
// 超限 → ResourceExhausted + Reason + RetryInfo。
func TestClientInvoke_QuotaWindows(t *testing.T) {
	cases := []struct {
		window    string
		bucketFmt string
		nextFmt   string
	}{
		{domainfunctions.ClientLimitWindowMinute, "200601021504", "200601021504"},
		{domainfunctions.ClientLimitWindowHour, "2006010215", "2006010215"},
		{domainfunctions.ClientLimitWindowDay, "20060102", "20060102"},
	}
	for _, tc := range cases {
		t.Run(tc.window, func(t *testing.T) {
			repo := newMockRepo()
			fn := seedClientCallableFunction(t, repo, "p1", "fn_1")
			fn.ClientLimitWindow = tc.window
			require.NoError(t, repo.UpdateFunction(context.Background(), fn))

			quota := &fakeQuotaLimiter{counts: []int64{1, 2, 5}} // 第 3 次 INCR=5 >= 5 超限
			uc := newTestUC(newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil), repo, newMockQueue())
			uc.clientQuota = quota

			cmd := ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"}

			// 前两次放行。
			for i := 0; i < 2; i++ {
				_, err := uc.ClientInvoke(endUserCtx(), cmd)
				require.NoError(t, err)
			}
			// 第三次超限。
			_, err := uc.ClientInvoke(endUserCtx(), cmd)
			require.Equal(t, codes.ResourceExhausted, status.Code(err))
			st, ok := status.FromError(err)
			require.True(t, ok)
			var reason *errdetails.ErrorInfo
			var retry *errdetails.RetryInfo
			for _, d := range st.Details() {
				if ei, ok := d.(*errdetails.ErrorInfo); ok {
					reason = ei
				}
				if ri, ok := d.(*errdetails.RetryInfo); ok {
					retry = ri
				}
			}
			require.NotNil(t, reason)
			require.Equal(t, "FUNCTIONS.INVOKE_QUOTA_EXCEEDED", reason.Reason)
			require.NotNil(t, retry, "RetryInfo 应指向窗口结束时刻")

			// 键形状：torchwood:fnq:{project}:{function}:{user}:{bucket}。
			// bucket 按当前时刻（UTC）与窗口粒度派生。
			require.Len(t, quota.keys, 3)
			nowUTC := time.Now().UTC()
			require.Equal(t, "torchwood:fnq:p1:fn_1:user-1:"+nowUTC.Format(tc.bucketFmt), quota.keys[0])
			require.Equal(t, []int{5, 5, 5}, quota.limits)
			// 窗口结束时刻 = bucket 起点 + 窗口长度（bucket 恰好整数对齐）。
			end := quota.ends[0].UTC()
			switch tc.window {
			case domainfunctions.ClientLimitWindowMinute:
				require.Equal(t, nowUTC.Truncate(time.Minute).Add(time.Minute).Format("200601021504"), end.Format(tc.nextFmt))
			case domainfunctions.ClientLimitWindowHour:
				require.Equal(t, nowUTC.Truncate(time.Hour).Add(time.Hour).Format("2006010215"), end.Format(tc.nextFmt))
			case domainfunctions.ClientLimitWindowDay:
				require.Equal(t, nowUTC.Truncate(24*time.Hour).Add(24*time.Hour).Format("20060102"), end.Format(tc.nextFmt))
			}
		})
	}
}

// TestClientInvoke_QuotaDBFallback：Redis 故障 → 按 function_executions 窗口内
// 计数（未超限放行 / 超限拒绝）；DB 亦不可用 → fail-closed（Unavailable）。
func TestClientInvoke_QuotaDBFallback(t *testing.T) {
	repo := newMockRepo()
	seedClientCallableFunction(t, repo, "p1", "fn_1")
	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
	uc := newTestUC(executor, repo, newMockQueue())
	uc.clientQuota = &fakeQuotaLimiter{allowErr: errors.New("redis down")}

	// 窗口内已有 1 次执行（<5）：放行并继续执行。
	require.NoError(t, repo.CreateExecution(context.Background(), &domainfunctions.ExecutionRecord{
		ID: "exe_prev", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_ready",
		Status: domainfunctions.ExecutionStatusCompleted, TriggerSource: domainfunctions.TriggerSourceClient,
		InvokingUserID: "user-1", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}))
	_, err := uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)

	// 超限（>=5）：ResourceExhausted。
	for i := 0; i < 4; i++ {
		require.NoError(t, repo.CreateExecution(context.Background(), &domainfunctions.ExecutionRecord{
			ID: "exe_fill_" + string(rune('a'+i)), FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_ready",
			Status: domainfunctions.ExecutionStatusCompleted, TriggerSource: domainfunctions.TriggerSourceClient,
			InvokingUserID: "user-1", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}))
	}
	_, err = uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	// server 面行不计入（trigger_source 过滤）。
	repo2 := newMockRepo()
	seedClientCallableFunction(t, repo2, "p1", "fn_1")
	uc2 := newTestUC(newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil), repo2, newMockQueue())
	uc2.clientQuota = &fakeQuotaLimiter{allowErr: errors.New("redis down")}
	for i := 0; i < 6; i++ {
		require.NoError(t, repo2.CreateExecution(context.Background(), &domainfunctions.ExecutionRecord{
			ID: "exe_srv_" + string(rune('a'+i)), FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_ready",
			Status:         domainfunctions.ExecutionStatusCompleted,
			InvokingUserID: "user-1", // 来源为空 = server 面，不应计数
			CreatedAt:      time.Now(), UpdatedAt: time.Now(),
		}))
	}
	_, err = uc2.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err, "server 面行不应计入 client 限频")

	// DB 亦不可用：fail-closed。
	dbErrRepo := &dbErrRepo{mockRepo: repo2, countErr: errors.New("db down")}
	uc3 := &Functions{
		cfg:         &config.AppConfig{},
		executor:    newMockExecutor(nil, nil),
		repo:        dbErrRepo,
		queue:       newMockQueue(),
		cache:       newFnCache(),
		clientQuota: &fakeQuotaLimiter{allowErr: errors.New("redis down")},
	}
	uc3.initUserGate(&config.AppConfig{})
	_, err = uc3.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

// dbErrRepo 在 mockRepo 之上注入 CountClientInvocations 故障。
type dbErrRepo struct {
	*mockRepo
	countErr error
}

func (r *dbErrRepo) CountClientInvocations(ctx context.Context, projectID, functionID, userID string, since time.Time) (int, error) {
	if r.countErr != nil {
		return 0, r.countErr
	}
	return r.mockRepo.CountClientInvocations(ctx, projectID, functionID, userID, since)
}

// idemRepo 在 mockRepo 之上模拟 partial 唯一索引冲突。
type idemRepo struct {
	*mockRepo
}

func (r *idemRepo) CreateExecution(_ context.Context, e *domainfunctions.ExecutionRecord) error {
	if e.ClientIdempotencyKey != "" {
		for _, existing := range r.mockRepo.executions {
			if existing.ProjectID == e.ProjectID && existing.FunctionID == e.FunctionID &&
				existing.InvokingUserID == e.InvokingUserID && existing.ClientIdempotencyKey == e.ClientIdempotencyKey {
				return domainfunctions.ErrExecutionIdempotencyConflict
			}
		}
	}
	return r.mockRepo.CreateExecution(context.Background(), e)
}

// TestClientInvoke_Idempotency：冲突返回既有行原样（running 与 completed 两态）；
// 未提供 key 行为不变。
func TestClientInvoke_Idempotency(t *testing.T) {
	for _, existingStatus := range []string{domainfunctions.ExecutionStatusRunning, domainfunctions.ExecutionStatusCompleted} {
		repo := &idemRepo{mockRepo: newMockRepo()}
		seedClientCallableFunction(t, repo.mockRepo, "p1", "fn_1")
		// 预置同键既有行。
		require.NoError(t, repo.mockRepo.CreateExecution(context.Background(), &domainfunctions.ExecutionRecord{
			ID: "exe_existing", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_ready",
			Status: existingStatus, TriggerSource: domainfunctions.TriggerSourceClient,
			InvokingUserID: "user-1", ClientIdempotencyKey: "idem-1",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}))
		executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
		uc := &Functions{
			cfg: &config.AppConfig{}, executor: executor, repo: repo, queue: newMockQueue(), cache: newFnCache(),
		}
		uc.initUserGate(&config.AppConfig{})

		res, err := uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1", IdempotencyKey: "idem-1"})
		require.NoError(t, err)
		require.True(t, res.Reused)
		require.Equal(t, "exe_existing", res.Record.ID)
		require.Equal(t, existingStatus, res.Record.Status, "命中幂等应原样返回既有状态")
		require.Empty(t, executor.calls, "幂等命中不得重新执行")
	}

	// 无既有行：正常创建并携带幂等字段。
	repo := &idemRepo{mockRepo: newMockRepo()}
	seedClientCallableFunction(t, repo.mockRepo, "p1", "fn_1")
	uc := &Functions{
		cfg: &config.AppConfig{}, executor: newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil), repo: repo, queue: newMockQueue(), cache: newFnCache(),
	}
	uc.initUserGate(&config.AppConfig{})
	res, err := uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1", IdempotencyKey: "idem-new"})
	require.NoError(t, err)
	require.False(t, res.Reused)
	stored, gerr := repo.GetExecution(context.Background(), "p1", "fn_1", res.Record.ID)
	require.NoError(t, gerr)
	require.Equal(t, "idem-new", stored.ClientIdempotencyKey)
	require.Equal(t, "user-1", stored.InvokingUserID)
	require.Equal(t, domainfunctions.TriggerSourceClient, stored.TriggerSource)
}

// TestClientInvoke_ConcurrencyGate：每用户第 3 个请求排队，队首超时 →
// ResourceExhausted；其他用户不受影响。
func TestClientInvoke_ConcurrencyGate(t *testing.T) {
	repo := newMockRepo()
	seedClientCallableFunction(t, repo, "p1", "fn_1")
	executor := newBlockingExecutor()
	uc := &Functions{
		cfg:         &config.AppConfig{},
		executor:    executor,
		repo:        repo,
		queue:       newMockQueue(),
		cache:       newFnCache(),
		clientQuota: &fakeQuotaLimiter{},
	}
	uc.initUserGate(&config.AppConfig{})
	uc.userGate = newUserGateLimiter(2, 100*time.Millisecond)

	// 占满 2 个槽：两个执行在途（executor 阻塞）。
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, _ = uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
			done <- struct{}{}
		}()
	}
	require.Eventually(t, func() bool { return executor.inFlight() == 2 }, 2*time.Second, 5*time.Millisecond)

	// 同用户第 3 个请求：排队超时。
	_, err := uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "per-user concurrency limit")

	// 释放在途执行后可再次获取槽位。
	executor.release()
	<-done
	<-done
	res, err := uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)
	require.NotNil(t, res)
	executor.release()
}

// blockingExecutor 是可控阻塞的 executor（并发闸门测试用）。
type blockingExecutor struct {
	mu      sync.Mutex
	blocked chan struct{}
	once    sync.Once
	calls   int
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{blocked: make(chan struct{})}
}

func (m *blockingExecutor) inFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *blockingExecutor) release() {
	m.once.Do(func() { close(m.blocked) })
}

func (m *blockingExecutor) Execute(_ context.Context, _ domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	<-m.blocked
	return &domainfunctions.ExecutionResult{StatusCode: 0}, nil
}

func (m *blockingExecutor) Build(context.Context, string, string, string) error { return nil }
func (m *blockingExecutor) RemoveImage(context.Context, string, string) error   { return nil }

// TestEgressClassification：client_callable=true 或存在 http/cron 触发器 →
// 不可信；纯 server 函数 → 可信。分类随 Execution 传给 executor。
func TestEgressClassification(t *testing.T) {
	// ① client_callable = true → untrusted。
	repo := newMockRepo()
	seedClientCallableFunction(t, repo, "p1", "fn_client")
	uc := newTestUC(newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil), repo, newMockQueue())
	uc.triggers = newMockTriggerRepo()
	uc.userGate = newUserGateLimiter(2, time.Second)
	uc.clientQuota = &fakeQuotaLimiter{}
	_, err := uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_client"})
	require.NoError(t, err)

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
	uc2 := newTestUC(executor, repo, newMockQueue())
	uc2.triggers = uc.triggers
	_, err = uc2.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_client"})
	require.NoError(t, err)
	require.True(t, executor.calls[0].EgressUntrusted, "client_callable=true 应分类为不可信")

	// ② 存在 http 触发器（server 面函数）→ untrusted。
	repo3 := newMockRepo()
	seedReadyFunction(repo3, "p1", "fn_trig", true, 15)
	trigs := newMockTriggerRepo()
	require.NoError(t, trigs.CreateTrigger(context.Background(), &domainfunctions.Trigger{
		ID: "trg-1", ProjectID: "p1", FunctionID: "fn_trig", Type: domainfunctions.TriggerTypeHTTP, Enabled: true,
	}))
	executor3 := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
	uc3 := newTestUC(executor3, repo3, newMockQueue())
	uc3.triggers = trigs
	_, err = uc3.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_trig"})
	require.NoError(t, err)
	require.True(t, executor3.calls[0].EgressUntrusted, "存在 http 触发器应分类为不可信")

	// ③ 纯 server 函数（无触发器、未开 client_callable）→ trusted。
	repo4 := newMockRepo()
	seedReadyFunction(repo4, "p1", "fn_server", true, 15)
	executor4 := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
	uc4 := newTestUC(executor4, repo4, newMockQueue())
	uc4.triggers = newMockTriggerRepo()
	_, err = uc4.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_server"})
	require.NoError(t, err)
	require.False(t, executor4.calls[0].EgressUntrusted, "纯 server 函数应分类为可信")

	// 分类缓存：函数删除触发器后 30s 缓存内仍按 untrusted（保守）。
	require.NoError(t, trigs.DeleteTrigger(context.Background(), "p1", "fn_trig", "trg-1"))
	_, err = uc3.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_trig"})
	require.NoError(t, err)
	require.True(t, executor3.calls[1].EgressUntrusted)
}

// TestClientManagement_AnonymousNotAllowed：Create/Update 遇
// client_anonymous_allowed=true 显式报错「一期未开放」。
func TestClientManagement_AnonymousNotAllowed(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateFunction(platformAdminCtx(), CreateFunctionCommand{
		ID: "fn_new", ProjectID: "p1", Name: "f", Runtime: "node-18.0",
		ClientAnonymousAllowed: boolPtr(true),
	})
	require.Error(t, err)
	require.Contains(t, status.Convert(err).Message(), "一期未开放")

	_, err = uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
		ProjectID: "p1", FunctionID: "fn_1",
		ClientAnonymousAllowed: boolPtr(true),
	})
	require.Error(t, err)
	require.Contains(t, status.Convert(err).Message(), "一期未开放")

	// client_callable=true 但 limit<1 → InvalidArgument。
	_, err = uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
		ProjectID: "p1", FunctionID: "fn_1", ClientCallable: boolPtr(true),
	})
	require.Error(t, err)
	require.Contains(t, status.Convert(err).Message(), "client_per_user_limit >= 1")

	// 合法开启：limit>=1 + 合法窗口。
	fn, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
		ProjectID: "p1", FunctionID: "fn_1", ClientCallable: boolPtr(true),
		ClientPerUserLimit: intPtr(3), ClientLimitWindow: strPtr("hour"),
	})
	require.NoError(t, err)
	require.True(t, fn.ClientCallable)
	require.Equal(t, 3, fn.ClientPerUserLimit)
	require.Equal(t, domainfunctions.ClientLimitWindowHour, fn.ClientLimitWindow)

	// 非法窗口 → InvalidArgument。
	_, err = uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
		ProjectID: "p1", FunctionID: "fn_1", ClientLimitWindow: strPtr("week"),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }
