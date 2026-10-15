package functions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/secretbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ——双轴调用面单测（v4 §4，D10）：fake Executor → fake Invoker/Notifier——

// fakeRepo 是 FunctionRepo 内嵌桩：记录 INSERT/UPDATE 与 CAS 行为。
type fakeRepo struct {
	domainfunctions.FunctionRepo
	fn           *domainfunctions.Function
	created      []*domainfunctions.InvocationRecord
	updated      []*domainfunctions.InvocationRecord
	getFnErr     error
	casFailFirst bool
	createErr    error
	byIdem       *domainfunctions.InvocationRecord
}

func (r *fakeRepo) CreateFunction(_ context.Context, fn *domainfunctions.Function) error {
	cp := *fn
	r.fn = &cp
	return nil
}

func (r *fakeRepo) UpdateFunction(_ context.Context, fn *domainfunctions.Function) error {
	cp := *fn
	r.fn = &cp
	return nil
}

func (r *fakeRepo) DeleteFunction(context.Context, string, string) error { return nil }

func (r *fakeRepo) GetFunction(_ context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	if r.getFnErr != nil {
		return nil, r.getFnErr
	}
	if r.fn == nil || r.fn.ProjectID != projectID || r.fn.ID != functionID {
		return nil, nil
	}
	return r.fn, nil
}

func (r *fakeRepo) CreateInvocation(_ context.Context, e *domainfunctions.InvocationRecord) error {
	if r.createErr != nil {
		return r.createErr
	}
	if e.ID == "" {
		e.ID = "inv-" + strings.ToLower(e.Status)
	}
	cp := *e
	r.created = append(r.created, &cp)
	return nil
}

func (r *fakeRepo) UpdateInvocation(_ context.Context, e *domainfunctions.InvocationRecord) error {
	cp := *e
	r.updated = append(r.updated, &cp)
	return nil
}

func (r *fakeRepo) GetInvocation(_ context.Context, _, _, _ string) (*domainfunctions.InvocationRecord, error) {
	if len(r.created) == 0 {
		return nil, nil
	}
	cp := *r.created[0]
	return &cp, nil
}

func (r *fakeRepo) TransitionInvocationStatus(_ context.Context, _, _, _ string, from, to string) (bool, error) {
	if r.casFailFirst {
		r.casFailFirst = false
		return false, nil
	}
	if len(r.created) == 0 || r.created[0].Status != from {
		return false, nil
	}
	r.created[0].Status = to
	return true, nil
}

func (r *fakeRepo) ListInvocations(context.Context, string, string, int, int, domainfunctions.InvocationListFilter) ([]domainfunctions.InvocationRecord, int, error) {
	return nil, 0, nil
}

func (r *fakeRepo) PruneOldInvocationsInProject(context.Context, string, string, int) error {
	return nil
}

func (r *fakeRepo) GetInvocationByIdempotencyKey(context.Context, string, string, string, string) (*domainfunctions.InvocationRecord, error) {
	return r.byIdem, nil
}

func (r *fakeRepo) CountClientInvocations(_ context.Context, projectID, functionID, userID string, since time.Time) (int, error) {
	// 限频 DB 降级路径的真实口径：窗口起点以来 client 来源计数（依 created 行）。
	n := 0
	for _, e := range r.created {
		if e.ProjectID == projectID && e.FunctionID == functionID &&
			e.TriggerSource == domainfunctions.TriggerSourceClient &&
			e.InvokingUserID == userID && !e.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

// fakeInvoker 捕获 RPC 轴调用入参并可注入结果/错误。
type fakeInvoker struct {
	invocations []domainfunctions.Invocation
	result      *domainfunctions.InvocationResult
	err         error
}

func (f *fakeInvoker) Invoke(_ context.Context, inv domainfunctions.Invocation) (*domainfunctions.InvocationResult, error) {
	f.invocations = append(f.invocations, inv)
	if f.err != nil {
		return f.result, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &domainfunctions.InvocationResult{Response: `{"ok":true}`, DurationMS: 5}, nil
}

// fakeNotifier 捕获事件轴投递入参并可注入结果/错误。
type fakeNotifier struct {
	notifications []domainfunctions.Notification
	result        domainfunctions.NotificationResult
	err           error
}

func (f *fakeNotifier) Notify(_ context.Context, n domainfunctions.Notification) (domainfunctions.NotificationResult, error) {
	f.notifications = append(f.notifications, n)
	if f.err != nil {
		return f.result, f.err
	}
	return f.result, nil
}

// fakeTokens 是执行身份端口桩：铸造可预测 token、记录吊销。
type fakeTokens struct {
	domainfunctions.ExecutionTokenService
	minted  []domainfunctions.ExecutionTokenInfo
	revoked []string
}

func (f *fakeTokens) Mint(_ context.Context, info domainfunctions.ExecutionTokenInfo, _ time.Duration) (string, error) {
	f.minted = append(f.minted, info)
	return "twx_test_" + info.ExecutionID, nil
}

func (f *fakeTokens) Revoke(_ context.Context, token string) error {
	f.revoked = append(f.revoked, token)
	return nil
}

// fakeQueue 捕获入队 payload。
type fakeQueue struct {
	payloads [][]byte
	err      error
}

func (q *fakeQueue) Trim(context.Context, string, int64) error { return nil }
func (q *fakeQueue) Enqueue(_ context.Context, _ string, payload []byte) error {
	if q.err != nil {
		return q.err
	}
	q.payloads = append(q.payloads, payload)
	return nil
}
func (q *fakeQueue) Dequeue(context.Context, string, time.Duration) ([]byte, string, error) {
	return nil, "", nil
}
func (q *fakeQueue) Ack(context.Context, string, string) error { return nil }

type fakeUsage struct {
	domainbilling.UsageCounter
	incremented int64
}

func (f *fakeUsage) Incr(context.Context, string, string, int64) error {
	f.incremented++
	return nil
}

func newTestFunctions(t *testing.T, fn *domainfunctions.Function, repo *fakeRepo, inv *fakeInvoker, notif *fakeNotifier, queue *fakeQueue) *Functions {
	t.Helper()
	if repo.fn == nil {
		repo.fn = fn
	}
	f := NewFunctions(&config.AppConfig{}, inv, notif, repo, queue, &fakeUsage{}, nil, &fakeTokens{}, nil, nil)
	// 测试密钥材料固定（initSecrets 默认回退 jwt.secret 为空——encryption
	// 走 secretbox 空密钥会拒绝，注入固定材料）。
	f.encKey = testEncKey
	return f
}

// testEncKey 是测试固定的 secretbox 密钥材料（与 newTestFunctions 注入值一致）。
const testEncKey = "test-enc-secret-0123456789abcdef"

func v4TestFunction() *domainfunctions.Function {
	// 真实加密落库形态（secretbox AES-256-GCM）：解密链全真。
	enc, err := secretbox.Encrypt("twfnsec_test", testEncKey)
	if err != nil {
		panic(err)
	}
	return &domainfunctions.Function{
		ID: "fn_1", ProjectID: "p1", Name: "fn",
		TargetURL: "https://fn.example.com", TimeoutSeconds: 15, Enabled: true,
		TargetSecretEnc: enc, TargetSecretFingerprint: "twfnsec_deadbeef", // #nosec G101 -- 测试夹具伪指纹（非凭证）
		DeclaredScopes: []string{"assets:write"},
	}
}

// serverCtx 构造 server 面 principal（G2-1 守卫要求）。
func serverCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:         "admin-1",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: true,
		UserID:          "admin-1",
		ProjectID:       "p1",
	})
}

// TestInvoke_RPCAxisInvokeMethod 同步调用走 RPC 轴 invoke：载荷直传、
// id=invocation id、身份头随载荷、终态 completed。
func TestInvoke_RPCAxisInvokeMethod(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	inv := &fakeInvoker{}
	notif := &fakeNotifier{}
	q := &fakeQueue{}
	f := newTestFunctions(t, fn, repo, inv, notif, q)

	tk := f.execTokens.(*fakeTokens)
	rec, err := f.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1", Data: `{"a":1}`})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusCompleted, rec.Status)
	require.Equal(t, `{"ok":true}`, rec.Response)

	require.Len(t, inv.invocations, 1)
	got := inv.invocations[0]
	require.Equal(t, domainfunctions.MethodInvoke, got.Method)
	require.Equal(t, `{"a":1}`, got.Data)
	require.Equal(t, rec.ID, got.ExecutionID, "JSON-RPC id = invocation id")
	require.Equal(t, "server", got.Source, "空 trigger_source 映射 server 字面值")
	require.NotEmpty(t, got.ExecutionToken, "执行身份随头注入")
	require.Equal(t, "https://fn.example.com", got.TargetURL)

	// mint 携带 declared_scopes 与 invoking_user（server 面为空）。
	require.Len(t, tk.minted, 1)
	require.Equal(t, []string{"assets:write"}, tk.minted[0].Scopes)
	// 调用结束主动吊销。
	require.Len(t, tk.revoked, 1)

	// 两写记账：INSERT running + UPDATE completed。
	require.Len(t, repo.created, 1)
	require.Equal(t, domainfunctions.InvocationStatusRunning, repo.created[0].Status)
	require.NotNil(t, repo.created[0].TimeoutSeconds, "行内超时快照（孤儿恢复 staleAfter 依据）")
	require.Len(t, repo.updated, 1)
	require.Equal(t, domainfunctions.InvocationStatusCompleted, repo.updated[0].Status)

	// 队列未被使用（同步快路径）。
	require.Empty(t, q.payloads)
}

// TestInvoke_HTTPRequestMethod http 触发器封套通道：method=http.request，
// params = {envelope, body_b64}。
func TestInvoke_HTTPRequestMethod(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	inv := &fakeInvoker{}
	f := newTestFunctions(t, fn, repo, inv, &fakeNotifier{}, &fakeQueue{})

	envelope := &domainfunctions.TriggerEnvelope{Method: "POST", Path: "/hook", RawQuery: "a=1"}
	rec, err := f.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID:       "p1",
		FunctionID:      "fn_1",
		Source:          domainfunctions.TriggerTypeHTTP + ":trg_1",
		SourceIP:        "192.0.2.1",
		TriggerEnvelope: envelope,
		RawBody:         []byte(`raw-body`),
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusCompleted, rec.Status)

	require.Len(t, inv.invocations, 1)
	got := inv.invocations[0]
	require.Equal(t, domainfunctions.MethodHTTPRequest, got.Method)
	require.Equal(t, envelope, got.TriggerEnvelope)
	require.Equal(t, []byte(`raw-body`), got.RawBody)
	require.Equal(t, "http:trg_1", got.Source)
	// 审计载体：SourceIP 落调用记录。
	require.Equal(t, "192.0.2.1", rec.SourceIP)
}

// TestInvoke_EventAxisCron cron 触发走事件轴：CloudEvent type=cron.tick、
// source=projects/{p}/triggers/{t}、data=CronTick 投影；成功=completed 且
// 响应尾部截断存档（D10 语义损失入册）。
func TestInvoke_EventAxisCron(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	notif := &fakeNotifier{result: domainfunctions.NotificationResult{ResponseTail: "tail", DurationMS: 7}}
	f := newTestFunctions(t, fn, repo, &fakeInvoker{}, notif, &fakeQueue{})

	scheduled := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	rec, err := f.InvokeTrigger(context.Background(), InvokeTriggerCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
		Data:       `{"trigger_id":"trg_c","scheduled_for":"2026-10-15T08:00:00Z"}`,
		Async:      true,
		Source:     domainfunctions.TriggerTypeCron + ":trg_c",
		EventAxis: &domainfunctions.EventNotificationMeta{
			Type: domainfunctions.CloudEventTypeCronTick,
			Time: scheduled,
		},
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusQueued, rec.Status, "cron 恒异步入队")

	// worker 领取后执行（repo 桩的 CAS 由 queued 状态命中）。
	require.NoError(t, f.ProcessInvocation(context.Background(), queueMessage{
		InvocationID: rec.ID,
		FunctionID:   "fn_1",
		ProjectID:    "p1",
		Data:         `{"trigger_id":"trg_c","scheduled_for":"2026-10-15T08:00:00Z"}`,
		Source:       domainfunctions.TriggerTypeCron + ":trg_c",
		EventAxis: &domainfunctions.EventNotificationMeta{
			Type: domainfunctions.CloudEventTypeCronTick,
			Time: scheduled,
		},
	}))

	require.Len(t, notif.notifications, 1)
	n := notif.notifications[0]
	require.Equal(t, domainfunctions.CloudEventTypeCronTick, n.Event.Type)
	require.Equal(t, "projects/p1/triggers/trg_c", n.Event.Source)
	require.Equal(t, "p1", n.Event.TwProject)
	require.Equal(t, int64(0), n.Event.TwSeq, "cron 自产事件无 seq")
	var tick domainfunctions.CronTick
	require.NoError(t, json.Unmarshal(n.Event.Data, &tick))
	require.Equal(t, "trg_c", tick.TriggerID)

	// 终态：Notify 成功 = completed；响应尾部截断存档。
	require.Len(t, repo.updated, 1)
	require.Equal(t, domainfunctions.InvocationStatusCompleted, repo.updated[0].Status)
	require.Equal(t, "tail", repo.updated[0].Response, "D10：响应尾部截断留档作审计替代")
	require.Equal(t, int64(7), repo.updated[0].DurationMS, "事件轴计时同样进 function_duration_ms")
}

// TestInvoke_EventAxisDocumentEvent 事件触发：type=事件目录名、source=
// projects/{p}、twseq 随信封。
func TestInvoke_EventAxisDocumentEvent(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	notif := &fakeNotifier{}
	f := newTestFunctions(t, fn, repo, &fakeInvoker{}, notif, &fakeQueue{})

	require.NoError(t, f.repo.CreateInvocation(context.Background(), &domainfunctions.InvocationRecord{
		ID: "inv-ev", FunctionID: "fn_1", ProjectID: "p1", Status: domainfunctions.InvocationStatusQueued,
		TriggerSource: domainfunctions.TriggerTypeEvent + ":trg_e",
	}))
	require.NoError(t, f.ProcessInvocation(context.Background(), queueMessage{
		InvocationID: "inv-ev",
		FunctionID:   "fn_1",
		ProjectID:    "p1",
		Data:         `{"type":"event","event_id":"ev-1","seq":42}`,
		Source:       domainfunctions.TriggerTypeEvent + ":trg_e",
		EventAxis: &domainfunctions.EventNotificationMeta{
			Type: "documents.created",
			Time: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC),
			Seq:  42,
		},
	}))

	require.Len(t, notif.notifications, 1)
	n := notif.notifications[0]
	require.Equal(t, "documents.created", n.Event.Type)
	require.Equal(t, "projects/p1", n.Event.Source, "文档/系统事件 source = projects/{p}")
	require.Equal(t, int64(42), n.Event.TwSeq, "事件面有 seq 则带")
	require.NotEmpty(t, n.ExecutionToken, "事件函数回访平台 API 的 twx_ 凭证通道")
}

// TestInvoke_PlatformErrorFailed 平台语义失败（连不通/超时）：failed + 错误
// 文本；超时归一固定文案。
func TestInvoke_PlatformErrorFailed(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	inv := &fakeInvoker{err: status.Error(codes.Unavailable, "dial refused")}
	f := newTestFunctions(t, fn, repo, inv, &fakeNotifier{}, &fakeQueue{})

	rec, err := f.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Error(t, err)
	require.Equal(t, domainfunctions.InvocationStatusFailed, rec.Status)
	require.Contains(t, rec.Error, "dial refused")

	// 超时：DeadlineExceeded 归一固定文案。
	inv2 := &fakeInvoker{err: status.Error(codes.DeadlineExceeded, "context deadline exceeded")}
	f2 := newTestFunctions(t, v4TestFunction(), &fakeRepo{fn: fn}, inv2, &fakeNotifier{}, &fakeQueue{})
	rec2, err := f2.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Error(t, err)
	require.Equal(t, "invocation timed out", rec2.Error)
}

// TestInvoke_FunctionErrorIsResult 函数应用层错误（JSON-RPC error 对象）：
// 送达成功、函数报错——记录标 failed 但 err==nil（调用方按 200+failed 消费）。
func TestInvoke_FunctionErrorIsResult(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	inv := &fakeInvoker{result: &domainfunctions.InvocationResult{
		FunctionError: &domainfunctions.RPCError{Code: -32000, Message: "boom"},
		DurationMS:    3,
	}}
	f := newTestFunctions(t, fn, repo, inv, &fakeNotifier{}, &fakeQueue{})

	rec, err := f.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err, "函数报错是合法结果，不映射传输错误")
	require.Equal(t, domainfunctions.InvocationStatusFailed, rec.Status)
	require.Equal(t, "boom", rec.Error)
}

// TestInvoke_FetchSemanticsTransparency http.request 的 HTTPResult 透传：
// StatusCode/Headers/ResponseB64 进运行期字段（不落库由 bun 映射排除）。
func TestInvoke_FetchSemanticsTransparency(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	inv := &fakeInvoker{result: &domainfunctions.InvocationResult{
		StatusCode:  201,
		Headers:     map[string]string{"content-type": "application/json"},
		ResponseB64: "e30=",
		Response:    "{}",
	}}
	f := newTestFunctions(t, fn, repo, inv, &fakeNotifier{}, &fakeQueue{})

	rec, err := f.Invoke(serverCtx(), InvokeCommand{
		ProjectID:       "p1",
		FunctionID:      "fn_1",
		TriggerEnvelope: &domainfunctions.TriggerEnvelope{Method: "GET", Path: "/x"},
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusCompleted, rec.Status)
	require.Equal(t, 201, rec.StatusCode, "自定义状态码是一等结果（D10）")
	require.Equal(t, "e30=", rec.ResponseB64)
	require.Len(t, rec.HTTPHeaders, 1)
}

// TestInvoke_Guards 调用面守卫矩阵：禁用函数、缺 target、data 上限、
// data 非 object、同步超时上限。
func TestInvoke_Guards(t *testing.T) {
	newFn := func() *domainfunctions.Function { return v4TestFunction() }

	// 禁用。
	repo := &fakeRepo{fn: newFn()}
	repo.fn.Enabled = false
	f := newTestFunctions(t, nil, repo, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	_, err := f.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// 缺 target（存量行 fail-closed）。
	repo2 := &fakeRepo{fn: newFn()}
	repo2.fn.TargetURL = ""
	f2 := newTestFunctions(t, nil, repo2, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	_, err = f2.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "no target endpoint")

	// data 超 32KB。
	f3 := newTestFunctions(t, newFn(), &fakeRepo{}, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	_, err = f3.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1", Data: `{"k":"` + strings.Repeat("x", 33<<10) + `"}`})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// data 非 object。
	_, err = f3.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1", Data: `[1,2]`})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 同步超时上限 30s。
	f4 := newTestFunctions(t, newFn(), &fakeRepo{}, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	f4.repo.(*fakeRepo).fn.TimeoutSeconds = 60
	_, err = f4.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "use async")

	// 函数不存在。
	_, err = f4.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestInvoke_AsyncQueueFlow 异步路径：queued 入队；入队失败标 failed。
func TestInvoke_AsyncQueueFlow(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	q := &fakeQueue{}
	f := newTestFunctions(t, fn, repo, &fakeInvoker{}, &fakeNotifier{}, q)

	rec, err := f.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1", Data: `{}`, Async: true})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusQueued, rec.Status)
	require.Len(t, q.payloads, 1)
	var msg queueMessage
	require.NoError(t, json.Unmarshal(q.payloads[0], &msg))
	require.Equal(t, rec.ID, msg.InvocationID)
	require.Equal(t, `{}`, msg.Data)

	// 入队失败：记录标 failed + Unavailable 上抛。
	q2 := &fakeQueue{err: errors.New("redis down")}
	f2 := newTestFunctions(t, v4TestFunction(), &fakeRepo{}, &fakeInvoker{}, &fakeNotifier{}, q2)
	_, err = f2.Invoke(serverCtx(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1", Async: true})
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Len(t, f2.repo.(*fakeRepo).updated, 1)
	require.Equal(t, domainfunctions.InvocationStatusFailed, f2.repo.(*fakeRepo).updated[0].Status)
	require.Equal(t, "enqueue failed", f2.repo.(*fakeRepo).updated[0].Error)
}

// TestProcessInvocation_Guards worker 领取守卫：CAS 未命中静默跳过、函数已删
// 标 failed。
func TestProcessInvocation_Guards(t *testing.T) {
	fn := v4TestFunction()

	// CAS 未命中（已被其他消费者领取）→ 静默跳过。
	repo := &fakeRepo{}
	repo.fn = fn
	f := newTestFunctions(t, fn, repo, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	require.NoError(t, f.repo.CreateInvocation(context.Background(), &domainfunctions.InvocationRecord{ID: "inv_x", FunctionID: "fn_1", ProjectID: "p1", Status: domainfunctions.InvocationStatusRunning}))
	repo.casFailFirst = true
	require.NoError(t, f.ProcessInvocation(context.Background(), queueMessage{InvocationID: "inv_x", FunctionID: "fn_1", ProjectID: "p1"}))
	require.Empty(t, f.invoker.(*fakeInvoker).invocations)

	// 函数已删除：终态失败。
	repo2 := &fakeRepo{}
	f2 := newTestFunctions(t, nil, repo2, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	require.NoError(t, f2.repo.CreateInvocation(context.Background(), &domainfunctions.InvocationRecord{ID: "inv_y", FunctionID: "gone", ProjectID: "p1", Status: domainfunctions.InvocationStatusQueued}))
	require.NoError(t, f2.ProcessInvocation(context.Background(), queueMessage{InvocationID: "inv_y", FunctionID: "gone", ProjectID: "p1"}))
	require.Len(t, repo2.updated, 1)
	require.Equal(t, domainfunctions.InvocationStatusFailed, repo2.updated[0].Status)
	require.Equal(t, "function not found", repo2.updated[0].Error)
}

// TestInvocation_TokenWithoutExecTokens execTokens 未装配（最小测试构造）：
// 调用照常进行、不注入身份头（declared_scopes 语义为增强而非执行前提）。
func TestInvocation_TokenWithoutExecTokens(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	inv := &fakeInvoker{}
	f := NewFunctions(&config.AppConfig{}, inv, &fakeNotifier{}, repo, &fakeQueue{}, nil, nil, nil, nil, nil)
	f.encKey = testEncKey
	// repo 直注函数（绕过 principal 守卫的 invokeInternal）。
	repo.fn = fn
	_, err := f.invokeInternal(context.Background(), InvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)
	require.Len(t, inv.invocations, 1)
	require.Empty(t, inv.invocations[0].ExecutionToken)
}

// TestRunnerSource 身份投影词表：空 → server；触发器前缀原样。
func TestRunnerSource(t *testing.T) {
	require.Equal(t, "server", runnerSource(""))
	require.Equal(t, "client", runnerSource("client"))
	require.Equal(t, "http:trg_1", runnerSource("http:trg_1"))
	require.Equal(t, "cron:trg_2", runnerSource("cron:trg_2"))
}

// TestInvocationDurationSource 双轴时长取值。
func TestInvocationDurationSource(t *testing.T) {
	require.Equal(t, int64(5), invocationDuration(&domainfunctions.InvocationResult{DurationMS: 5}, domainfunctions.NotificationResult{DurationMS: 9}))
	require.Equal(t, int64(9), invocationDuration(nil, domainfunctions.NotificationResult{DurationMS: 9}))
}

// ListInvocations 过滤校验：未知 status 拒绝。
func TestListInvocations_StatusGuard(t *testing.T) {
	fn := v4TestFunction()
	repo := &fakeRepo{}
	f := newTestFunctions(t, fn, repo, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	_, _, err := f.ListInvocations(context.Background(), "p1", "fn_1", 0, "", domainfunctions.InvocationListFilter{Status: "building"})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "building 态随补构建退役")
	_, _, err = f.ListInvocations(context.Background(), "p1", "fn_1", 0, "", domainfunctions.InvocationListFilter{Status: domainfunctions.InvocationStatusRunning})
	require.NoError(t, err)
}

// endUserCtx 构造端用户 principal（P2 客户端调用面守卫要求）。
func endUserCtx(userID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:   idgen.ID(userID),
		ActorKind: shared.ActorKindEndUser,
		UserID:    userID,
		ProjectID: "p1",
	})
}

// 接口实现守卫。
var (
	_ shared.Queue             = (*fakeQueue)(nil)
	_ domainfunctions.Invoker  = (*fakeInvoker)(nil)
	_ domainfunctions.Notifier = (*fakeNotifier)(nil)
)
