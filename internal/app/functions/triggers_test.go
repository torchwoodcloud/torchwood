package functions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockTriggerRepo 是 TriggerRepo 的内存实现（测试用）；CAS 语义按
// next_run_at 逐行比对，供领取并发/幂等断言。
type mockTriggerRepo struct {
	mu        sync.Mutex
	triggers  map[string]*domainfunctions.Trigger
	claimErr  error
	claimLog  []string
	createErr error
}

func newMockTriggerRepo() *mockTriggerRepo {
	return &mockTriggerRepo{triggers: map[string]*domainfunctions.Trigger{}}
}

func (r *mockTriggerRepo) CreateTrigger(_ context.Context, t *domainfunctions.Trigger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return r.createErr
	}
	cp := *t
	r.triggers[t.ID] = &cp
	return nil
}

func (r *mockTriggerRepo) GetTrigger(_ context.Context, projectID, functionID, triggerID string) (*domainfunctions.Trigger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.triggers[triggerID]
	if t == nil || t.ProjectID != projectID || t.FunctionID != functionID {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (r *mockTriggerRepo) ListTriggers(_ context.Context, projectID, functionID string) ([]domainfunctions.Trigger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domainfunctions.Trigger
	for _, t := range r.triggers {
		if t.ProjectID == projectID && t.FunctionID == functionID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (r *mockTriggerRepo) UpdateTrigger(_ context.Context, t *domainfunctions.Trigger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.triggers[t.ID]; !ok {
		return nil
	}
	cp := *t
	r.triggers[t.ID] = &cp
	return nil
}

func (r *mockTriggerRepo) DeleteTrigger(_ context.Context, projectID, functionID, triggerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.triggers[triggerID]
	if t != nil && t.ProjectID == projectID && t.FunctionID == functionID {
		delete(r.triggers, triggerID)
	}
	return nil
}

func (r *mockTriggerRepo) GetTriggerByToken(_ context.Context, projectID, token string) (*domainfunctions.Trigger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.triggers {
		if t.Token == token && t.ProjectID == projectID {
			cp := *t
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *mockTriggerRepo) ClaimDueCron(ctx context.Context, projectID string, now time.Time, limit int, next domainfunctions.CronNextFunc) ([]domainfunctions.CronClaim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	var claims []domainfunctions.CronClaim
	for _, t := range r.triggers {
		if len(claims)+len(r.claimLog) >= limit {
			break
		}
		if t.ProjectID != projectID || !t.Enabled || t.Type != domainfunctions.TriggerTypeCron {
			continue
		}
		if t.NextRunAt == nil || t.NextRunAt.After(now) {
			continue
		}
		due := *t.NextRunAt
		plan, err := next(domainfunctions.CronNextInput{Due: due, Now: now, Expr: t.Config.Expr, Misfire: t.Config.Misfire})
		if err != nil {
			continue
		}
		nt := *t
		nt.NextRunAt = &plan.Next
		r.triggers[t.ID] = &nt
		r.claimLog = append(r.claimLog, t.ID)
		if !plan.Run {
			continue
		}
		claims = append(claims, domainfunctions.CronClaim{
			ProjectID: projectID, FunctionID: t.FunctionID, TriggerID: t.ID, ScheduledFor: due,
		})
	}
	return claims, nil
}

func newTriggerTestUC(executor *mockExecutor, repo *mockRepo, queue *mockQueue, triggers *mockTriggerRepo, projects domainprojects.Repository) *Functions {
	uc := NewFunctionsWithUsage(&config.AppConfig{}, executor, repo, queue, nil, projects, Semaphores{}, nil, triggers)
	return uc
}

func TestCreateFunctionTrigger_HTTPGeneratesTokenAndValidates(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	triggers := newMockTriggerRepo()
	uc := newTriggerTestUC(newMockExecutor(nil, nil), repo, newMockQueue(), triggers, nil)

	trg, err := uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeAsyncAck, AckBody: `{"is_valid":true}`, Handshake: domainfunctions.HandshakeEcho},
	})
	require.NoError(t, err)
	require.NotEmpty(t, trg.Token, "token 服务端生成")
	require.Len(t, trg.Token, 22, "128bit base64url")
	require.Equal(t, "http:"+trg.ID, trg.TriggerSource())

	// response_mode 必填。
	_, err = uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// ack_body > 1KB 拒绝（防公开端点带宽放大）。
	_, err = uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{
			ResponseMode: domainfunctions.ResponseModeAsyncAck,
			AckBody:      strings.Repeat("a", domainfunctions.MaxAckBodyBytes+1),
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "ack_body 超限创建被拒")

	// body_limit > 1MB 拒绝。
	_, err = uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{
			ResponseMode:   domainfunctions.ResponseModeSync,
			BodyLimitBytes: domainfunctions.MaxHTTPBodyLimitBytes + 1,
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// handshake 词表。
	_, err = uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeSync, Handshake: "tsign"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 函数不存在 → 404。
	_, err = uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_missing", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeSync},
	})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestCreateFunctionTrigger_CronParsesExprAndSetsNextRun(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	triggers := newMockTriggerRepo()
	uc := newTriggerTestUC(newMockExecutor(nil, nil), repo, newMockQueue(), triggers, nil)

	trg, err := uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeCron,
		Cron: domainfunctions.TriggerConfig{Expr: "0 3 * * *"},
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.MisfireCatchUpOnce, trg.Config.Misfire, "misfire 缺省 catch_up_once")
	require.NotNil(t, trg.NextRunAt)
	require.True(t, trg.NextRunAt.After(time.Now().Add(-time.Second)), "next_run_at = now 之后的下一计划时刻")
	require.Equal(t, time.UTC.String(), trg.NextRunAt.Location().String(), "cron 一期 UTC")

	// 坏表达式拒绝。
	_, err = uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeCron,
		Cron: domainfunctions.TriggerConfig{Expr: "61 * * * *"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestRotateFunctionTriggerToken(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	triggers := newMockTriggerRepo()
	uc := newTriggerTestUC(newMockExecutor(nil, nil), repo, newMockQueue(), triggers, nil)

	trg, err := uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeSync},
	})
	require.NoError(t, err)
	old := trg.Token

	rotated, err := uc.RotateFunctionTriggerToken(platformAdminCtx(), "p1", "fn_1", trg.ID)
	require.NoError(t, err)
	require.NotEqual(t, old, rotated.Token, "轮换产生新 token")
	require.NotEmpty(t, old)

	// cron 触发器不支持轮换。
	cronTrg, err := uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeCron,
		Cron: domainfunctions.TriggerConfig{Expr: "* * * * *"},
	})
	require.NoError(t, err)
	_, err = uc.RotateFunctionTriggerToken(platformAdminCtx(), "p1", "fn_1", cronTrg.ID)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 旧 token 不再命中（fail-closed）。
	got, err := uc.GetHTTPTriggerByToken(context.Background(), "p1", old)
	require.NoError(t, err)
	require.Nil(t, got, "旧 token 轮换后立即失效")
	got, err = uc.GetHTTPTriggerByToken(context.Background(), "p1", rotated.Token)
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestInvokeTrigger_SyncRecordsSourceAndPassthrough(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	triggers := newMockTriggerRepo()
	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Response: `{"ok":1}`}, nil)
	uc := newTriggerTestUC(executor, repo, newMockQueue(), triggers, nil)

	trg, err := uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeSync},
	})
	require.NoError(t, err)

	rec, err := uc.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1",
		Data:   `{"method":"POST"}`,
		Source: trg.TriggerSource(), SourceIP: "203.0.113.7",
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, rec.Status)
	require.Equal(t, "http:"+trg.ID, rec.TriggerSource, "执行记录记 trigger_source（审计载体）")
	require.Equal(t, "203.0.113.7", rec.SourceIP)

	// 同一封套经异步路径：入队成功（queue fake 注入失败路径见下一个测试）。
	recAsync, err := uc.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1",
		Data: `{"method":"POST"}`, Async: true, Source: trg.TriggerSource(), SourceIP: "203.0.113.7",
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusQueued, recAsync.Status)
	require.Equal(t, trg.TriggerSource(), recAsync.TriggerSource)
}

// TestInvokeTrigger_AsyncAckOrderRedLine 顺序红线（设计 §3 三轮复核）：
// 入队失败 → InvokeTrigger 返回错误（执行记录标 failed），handler 据此写
// 5xx——绝不出现「先 200 后入队失败」的事件丢失窗口。
func TestInvokeTrigger_AsyncAckOrderRedLine(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	triggers := newMockTriggerRepo()
	queue := newMockQueue()
	queue.err = errors.New("redis down")
	uc := newTriggerTestUC(newMockExecutor(nil, nil), repo, queue, triggers, nil)

	trg, err := uc.CreateFunctionTrigger(platformAdminCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeAsyncAck, AckBody: `{"is_valid":true}`},
	})
	require.NoError(t, err)

	_, err = uc.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1",
		Data: `{"method":"POST"}`, Async: true, Source: trg.TriggerSource(),
	})
	require.Error(t, err, "入队失败必须上抛（handler 写 5xx）")
	require.Len(t, queue.enqueued, 0)

	// 执行记录兜底标 failed（审计不留悬空 queued）。
	require.Len(t, repo.executions, 1)
	for _, e := range repo.executions {
		require.Equal(t, domainfunctions.ExecutionStatusFailed, e.Status, "入队失败的执行记录标 failed")
		require.Equal(t, trg.TriggerSource(), e.TriggerSource)
	}
}

// TestInvokeTrigger_TriggerBodyLimitRelaxed v2 dispatcher 模式下触发器封套
// 上限放宽至 body_limit（≤1MB）；v1（默认 docker）保持 32KB env 通道上限。
func TestInvokeTrigger_TriggerBodyLimitRelaxed(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	triggers := newMockTriggerRepo()
	uc := newTriggerTestUC(newMockExecutor(nil, nil), repo, newMockQueue(), triggers, nil)

	// v1：>32KB data 拒绝（env 通道）。
	big := fmt.Sprintf(`{"body":%q}`, strings.Repeat("x", 40<<10))
	_, err := uc.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Data: big, Async: true,
		Source: "http:trg_x", BodyLimitBytes: 64 << 10,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "v1 env 通道保持 32KB 上限")

	// v2：同 data 放行（body 通道，触发器上限生效）。
	ucV2 := NewFunctionsWithUsage(&config.AppConfig{Functions: &config.Functions{Executor: "dispatcher"}}, newMockExecutor(nil, nil), repo, newMockQueue(), nil, nil, Semaphores{}, nil, triggers)
	_, err = ucV2.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Data: big, Async: true,
		Source: "http:trg_x", BodyLimitBytes: 64 << 10,
	})
	require.NoError(t, err, "v2 body 通道放宽到触发器上限")
}

// TestMaxTriggerBodyLimit 锁 v1/v2 执行器模式下的生效 body 上限语义。
func TestMaxTriggerBodyLimit(t *testing.T) {
	v1 := NewFunctions(&config.AppConfig{}, nil, nil, nil)
	// v1 env 通道：封套余量 8KB，32KB data 预算 → 24KB（缺省 64KB 同样收窄
	// ——物理上 data 经 TW_DATA 环境变量注入，超 32KB 无法 execve）。
	require.Equal(t, 24<<10, v1.MaxTriggerBodyLimit(0))
	require.Equal(t, 24<<10, v1.MaxTriggerBodyLimit(64<<10))
	v2 := NewFunctions(&config.AppConfig{Functions: &config.Functions{Executor: "dispatcher"}}, nil, nil, nil)
	require.Equal(t, 64<<10, v2.MaxTriggerBodyLimit(0), "v2 缺省 64KB")
	require.Equal(t, 1<<20, v2.MaxTriggerBodyLimit(1<<20), "v2 body 通道配置全额生效")
	require.Equal(t, 1<<20, v2.MaxTriggerBodyLimit(2<<20), "超过 1MB 收到 1MB")
}

// TestDispatchDueCronTriggers_EnqueueFailureUndoClaim 入队失败回滚领取：
// next_run_at 回滚到原到期值，下轮扫描 catch_up 语义兜底重领；批内其他
// 触发器不受阻塞。
func TestDispatchDueCronTriggers_EnqueueFailureUndoClaim(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	triggers := newMockTriggerRepo()
	queue := newMockQueue()
	queue.err = errors.New("redis down")
	uc := newTriggerTestUC(newMockExecutor(nil, nil), repo, queue, triggers, &stubProjectRepo{
		list: []domainprojects.Project{{ID: "p1", Status: "active"}},
	})

	now := time.Now()
	due := now.Add(-30 * time.Second)
	trg := &domainfunctions.Trigger{
		ID: "trg_c1", ProjectID: "p1", FunctionID: "fn_1",
		Type:    domainfunctions.TriggerTypeCron,
		Config:  domainfunctions.TriggerConfig{Expr: "* * * * *", Misfire: domainfunctions.MisfireCatchUpOnce},
		Enabled: true, NextRunAt: &due,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, triggers.CreateTrigger(context.Background(), trg))

	n, err := uc.DispatchDueCronTriggers(context.Background(), now, 100)
	require.NoError(t, err)
	require.Equal(t, 0, n, "入队失败不计投递")

	// 领取已回滚：next_run_at 回到原到期值（下轮重领）。
	got := triggers.triggers["trg_c1"]
	require.NotNil(t, got.NextRunAt)
	require.True(t, got.NextRunAt.Equal(due), "入队失败回滚 next_run_at 到原到期值")
}
