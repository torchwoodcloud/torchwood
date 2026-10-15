package assets

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	infraauth "github.com/torchwoodcloud/torchwood/internal/infra/auth"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 全路径身份断言（P0 执行身份 × P2 客户端调用面）：真实 Redis token 服务
// （miniredis）之上的 mint（经 Functions.ClientInvoke，携带调用记录的
// InvokingUserID）→ Validate（真 Validator 构造 execution principal）→
// operatorFrom 输出。operator_execution_test.go 直接手工构造 principal，
// 证明不了 mint 侧是否真的把调用用户带进链路——本测试补上这一环。
//
// 函数仓储用接口内嵌桩（ClientInvoke 同步路径只触达下列四个方法，其余
// 方法不被调用）。Invoker 桩捕获随签名请求头注入的执行身份 token（v4：
// X-Tw-Execution-Token——常驻的是服务不是凭证；函数在调用期间拿它回访
// Server API）。
type fullpathFnRepo struct {
	domainfunctions.FunctionRepo
	fn *domainfunctions.Function
}

func (r *fullpathFnRepo) GetFunction(_ context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	if r.fn == nil || r.fn.ProjectID != projectID || r.fn.ID != functionID {
		return nil, nil
	}
	return r.fn, nil
}

func (r *fullpathFnRepo) CreateInvocation(_ context.Context, e *domainfunctions.InvocationRecord) error {
	e.ID = "inv_fullpath"
	return nil
}

func (r *fullpathFnRepo) UpdateInvocation(context.Context, *domainfunctions.InvocationRecord) error {
	return nil
}

func (r *fullpathFnRepo) CountClientInvocations(context.Context, string, string, string, time.Time) (int, error) {
	return 0, nil
}

type fullpathInvoker struct {
	// onInvoke 在调用进行中回调（真实时序：函数 endpoint 是在调用期间拿
	// X-Tw-Execution-Token 头回访 Server API；调用结束 defer 即吊销，返回后
	// Validate 必 401）。
	onInvoke func(execToken string)
}

func (m *fullpathInvoker) Invoke(_ context.Context, inv domainfunctions.Invocation) (*domainfunctions.InvocationResult, error) {
	if m.onInvoke != nil {
		m.onInvoke(inv.ExecutionToken)
	}
	return &domainfunctions.InvocationResult{}, nil
}

type fullpathNotifier struct{}

func (fullpathNotifier) Notify(context.Context, domainfunctions.Notification) (domainfunctions.NotificationResult, error) {
	return domainfunctions.NotificationResult{}, nil
}

type fullpathQueue struct{}

func (fullpathQueue) Trim(context.Context, string, int64) error     { return nil }
func (fullpathQueue) Enqueue(context.Context, string, []byte) error { return nil }
func (fullpathQueue) Dequeue(context.Context, string, time.Duration) ([]byte, string, error) {
	return nil, "", nil
}
func (fullpathQueue) Ack(context.Context, string, string) error { return nil }

func TestExecutionIdentity_FullPathMintValidateOperator(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	tokenSvc := infrafunctions.NewRedisExecutionTokenService(rdb)

	fn := &domainfunctions.Function{
		ID: "fn_full", ProjectID: "p1", Name: "fn",
		TargetURL: "https://fn.example.com", TimeoutSeconds: 15, Enabled: true,
		ClientCallable: true, ClientPerUserLimit: 5, ClientLimitWindow: domainfunctions.ClientLimitWindowDay,
		DeclaredScopes: []string{"assets:write"},
	}
	invoker := &fullpathInvoker{}
	validator := infraauth.NewValidatorWithOneTimeTokens(
		&config.AppConfig{Security: &config.Security{Jwt: &config.Security_Jwt{Secret: "test-secret"}}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, tokenSvc,
	)
	var (
		execToken  string
		operatorOk bool
		snap       domainassets.OperatorSnapshot
	)
	invoker.onInvoke = func(token string) {
		execToken = token
		// ② Validate：函数 endpoint 回访 Server API 时真 Validator 构造
		// execution principal——invoking_user 必须穿透 Redis 投影回来。
		p, err := validator.ValidateCredential(context.Background(), token, domainshared.CredentialTypeExecution)
		require.NoError(t, err)
		require.Equal(t, domainshared.ActorKindExecution, p.ActorKind)
		require.Equal(t, "fn_full", p.FunctionID)
		require.Equal(t, "p1", p.ProjectID)
		require.Equal(t, "user-1", p.InvokingUserID, "mint 必须把调用用户写进 token 投影")

		// ③ operatorFrom：资产账本 operator 快照 = function + invoking user
		// 双维溯源（对标 PlayFab currentPlayerId）。
		ledgerCtx := contexts.WithPrincipal(context.Background(), p)
		require.NoError(t, json.Unmarshal(operatorFrom(ledgerCtx), &snap))
		require.Equal(t, "execution", snap.ActorKind)
		require.Equal(t, "fn_full", snap.ActorID)
		require.Equal(t, "user-1", snap.UserID)
		require.Equal(t, "execution", snap.CredentialType)
		operatorOk = true
	}
	fnUC := appfunctions.NewFunctions(
		&config.AppConfig{}, invoker, fullpathNotifier{}, &fullpathFnRepo{fn: fn},
		fullpathQueue{}, nil, nil, tokenSvc, nil, nil,
	)

	// ① mint：端用户经客户端调用面触发调用（真实 token 服务落 Redis 投影）。
	userCtx := contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorID:   "user-1",
		ActorKind: domainshared.ActorKindEndUser,
		UserID:    "user-1",
		ProjectID: "p1",
	})
	_, err := fnUC.ClientInvoke(userCtx, appfunctions.ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_full"})
	require.NoError(t, err)

	require.True(t, operatorOk, "调用中回调必须已走完 Validate → operatorFrom 断言")
	require.NotEmpty(t, execToken, "执行身份 token 应随 X-Tw-Execution-Token 头注入")

	// 调用结束即主动吊销：ClientInvoke 返回（defer 已 DEL）后同 token 二次
	// 校验 401——「常驻的是服务不是凭证」。
	_, err = validator.ValidateCredential(context.Background(), execToken, domainshared.CredentialTypeExecution)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
