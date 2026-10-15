package functions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ——触发器面单测（P1 触发器模块；v4 cron 领取走事件轴原料 D10）——

// stubTriggerRepo 是 TriggerRepo 桩：记录写入并支持注入领取结果。
type stubTriggerRepo struct {
	domainfunctions.TriggerRepo
	trg        *domainfunctions.Trigger
	claims     []domainfunctions.CronClaim
	claimErr   error
	updated    []*domainfunctions.Trigger
	created    []*domainfunctions.Trigger
	claimCount int
}

func (r *stubTriggerRepo) CreateTrigger(_ context.Context, t *domainfunctions.Trigger) error {
	cp := *t
	r.created = append(r.created, &cp)
	r.trg = &cp
	return nil
}

func (r *stubTriggerRepo) GetTrigger(_ context.Context, projectID, functionID, triggerID string) (*domainfunctions.Trigger, error) {
	if r.trg == nil || r.trg.ProjectID != projectID || r.trg.FunctionID != functionID || r.trg.ID != triggerID {
		return nil, nil
	}
	cp := *r.trg
	return &cp, nil
}

func (r *stubTriggerRepo) ListTriggers(context.Context, string, string) ([]domainfunctions.Trigger, error) {
	if r.trg == nil {
		return nil, nil
	}
	return []domainfunctions.Trigger{*r.trg}, nil
}

func (r *stubTriggerRepo) UpdateTrigger(_ context.Context, t *domainfunctions.Trigger) error {
	cp := *t
	r.updated = append(r.updated, &cp)
	r.trg = &cp
	return nil
}

func (r *stubTriggerRepo) DeleteTrigger(context.Context, string, string, string) error { return nil }

func (r *stubTriggerRepo) GetTriggerByToken(_ context.Context, projectID, token string) (*domainfunctions.Trigger, error) {
	if r.trg != nil && r.trg.ProjectID == projectID && r.trg.Token == token && r.trg.Enabled && r.trg.Type == domainfunctions.TriggerTypeHTTP {
		return r.trg, nil
	}
	return nil, nil
}

func (r *stubTriggerRepo) ClaimDueCron(context.Context, string, time.Time, int, domainfunctions.CronNextFunc) ([]domainfunctions.CronClaim, error) {
	r.claimCount++
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	return r.claims, nil
}

func (r *stubTriggerRepo) ListEnabledEventTriggers(context.Context, string) ([]domainfunctions.Trigger, error) {
	return nil, nil
}

// dirStub 是项目目录桩（cron 领取的项目枚举面）。
type dirStub struct {
	domainprojects.Repository
	ids []string
}

func (d dirStub) ListProjects(context.Context) ([]domainprojects.Project, error) {
	out := make([]domainprojects.Project, len(d.ids))
	for i, id := range d.ids {
		out[i] = domainprojects.Project{ID: id, Status: "active"}
	}
	return out, nil
}

func newTriggersFunctions(t *testing.T, repo *stubTriggerRepo, queue *fakeQueue) *Functions {
	t.Helper()
	return newTriggersFunctionsWithDir(t, repo, queue, dirStub{ids: []string{"p1"}})
}

func newTriggersFunctionsWithDir(t *testing.T, repo *stubTriggerRepo, queue *fakeQueue, dir domainprojects.Repository) *Functions {
	t.Helper()
	f := NewFunctions(&config.AppConfig{}, &fakeInvoker{}, &fakeNotifier{}, &fakeRepo{fn: v4TestFunction()}, queue, nil, dir, &fakeTokens{}, repo, nil)
	f.encKey = testEncKey
	return f
}

func TestCreateFunctionTrigger_HTTP(t *testing.T) {
	repo := &stubTriggerRepo{}
	f := newTriggersFunctions(t, repo, &fakeQueue{})

	trg, err := f.CreateFunctionTrigger(serverCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		HTTP: domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeSync},
	})
	require.NoError(t, err)
	require.NotEmpty(t, trg.Token, "http token 服务端生成（128bit，URL 即鉴权）")
	require.Len(t, repo.created, 1)

	// response_mode 必填。
	_, err = f.CreateFunctionTrigger(serverCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestCreateFunctionTrigger_Cron(t *testing.T) {
	repo := &stubTriggerRepo{}
	f := newTriggersFunctions(t, repo, &fakeQueue{})

	trg, err := f.CreateFunctionTrigger(serverCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeCron,
		Cron: domainfunctions.TriggerConfig{Expr: "*/5 * * * *"},
	})
	require.NoError(t, err)
	require.NotNil(t, trg.NextRunAt, "cron 创建即计算下一计划时刻")
	require.True(t, trg.NextRunAt.After(time.Now()), "推进不变量：下一计划时刻严格大于 now")
	require.True(t, trg.NextRunAt.Before(time.Now().Add(6*time.Minute)))

	// 非法表达式拒绝。
	_, err = f.CreateFunctionTrigger(serverCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeCron,
		Cron: domainfunctions.TriggerConfig{Expr: "not-a-cron"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestCreateFunctionTrigger_Event(t *testing.T) {
	repo := &stubTriggerRepo{}
	f := newTriggersFunctions(t, repo, &fakeQueue{})

	trg, err := f.CreateFunctionTrigger(serverCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeEvent,
		Events: []string{"databases.app.collections.notes.documents.create", "auth.users.created"},
	})
	require.NoError(t, err)
	require.Len(t, trg.Config.Events, 2)

	// 目录外前缀 fail-closed。
	_, err = f.CreateFunctionTrigger(serverCtx(), CreateTriggerCommand{
		ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeEvent,
		Events: []string{"bogus.*"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestRotateFunctionTriggerToken(t *testing.T) {
	repo := &stubTriggerRepo{trg: &domainfunctions.Trigger{
		ID: "trg_1", ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		Token: "old", Enabled: true,
	}}
	f := newTriggersFunctions(t, repo, &fakeQueue{})

	trg, err := f.RotateFunctionTriggerToken(serverCtx(), "p1", "fn_1", "trg_1")
	require.NoError(t, err)
	require.NotEqual(t, "old", trg.Token, "旧 token 立即失效")
	require.Len(t, repo.updated, 1)

	// cron 触发器无 token 可轮换。
	repo2 := &stubTriggerRepo{trg: &domainfunctions.Trigger{
		ID: "trg_c", ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeCron, Enabled: true,
	}}
	f2 := newTriggersFunctions(t, repo2, &fakeQueue{})
	_, err = f2.RotateFunctionTriggerToken(serverCtx(), "p1", "fn_1", "trg_c")
	require.ErrorContains(t, err, "http triggers only")
}

func TestGetHTTPTriggerByToken(t *testing.T) {
	repo := &stubTriggerRepo{trg: &domainfunctions.Trigger{
		ID: "trg_1", ProjectID: "p1", FunctionID: "fn_1", Type: domainfunctions.TriggerTypeHTTP,
		Token: "tok", Enabled: true,
	}}
	f := newTriggersFunctions(t, repo, &fakeQueue{})

	// 命中（不要求 principal——token 即鉴权）。
	trg, err := f.GetHTTPTriggerByToken(context.Background(), "p1", "tok")
	require.NoError(t, err)
	require.NotNil(t, trg)

	// 禁用/跨项目探测 → nil（不泄露存在性）。
	repo.trg.Enabled = false
	trg, err = f.GetHTTPTriggerByToken(context.Background(), "p1", "tok")
	require.NoError(t, err)
	require.Nil(t, trg)
}

// TestDispatchDueCronTriggers cron 领取 → 事件轴原料入队：data = CronTick
// 投影、EventAxis{type: cron.tick, time: scheduled_for}。
func TestDispatchDueCronTriggers(t *testing.T) {
	repo := &stubTriggerRepo{}
	q := &fakeQueue{}
	f := newTriggersFunctions(t, repo, q)

	scheduled := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	repo.claims = []domainfunctions.CronClaim{{
		ProjectID: "p1", FunctionID: "fn_1", TriggerID: "trg_c", ScheduledFor: scheduled,
	}}
	n, err := f.DispatchDueCronTriggers(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.Len(t, q.payloads, 1)
	var msg queueMessage
	require.NoError(t, json.Unmarshal(q.payloads[0], &msg))
	require.Equal(t, domainfunctions.TriggerTypeCron+":trg_c", msg.Source)
	require.NotNil(t, msg.EventAxis)
	require.Equal(t, domainfunctions.CloudEventTypeCronTick, msg.EventAxis.Type)
	require.Equal(t, scheduled.UTC(), msg.EventAxis.Time.UTC())
	var tick domainfunctions.CronTick
	require.NoError(t, json.Unmarshal([]byte(msg.Data), &tick))
	require.Equal(t, "trg_c", tick.TriggerID)
	require.Equal(t, scheduled.UTC().Format(time.RFC3339), tick.ScheduledFor)
}

// TestMaxTriggerBodyLimit body 上限钳制：0 → 缺省 64KB；>1MB → 钳到 1MB。
func TestMaxTriggerBodyLimit(t *testing.T) {
	repo := &stubTriggerRepo{}
	f := newTriggersFunctions(t, repo, &fakeQueue{})
	require.Equal(t, domainfunctions.DefaultHTTPBodyLimitBytes, f.MaxTriggerBodyLimit(0))
	require.Equal(t, domainfunctions.MaxHTTPBodyLimitBytes, f.MaxTriggerBodyLimit(2<<20))
	require.Equal(t, 1024, f.MaxTriggerBodyLimit(1024))
}
