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
// （miniredis）之上的 mint（经 Functions.ClientInvoke，携带执行记录的
// InvokingUserID）→ Validate（真 Validator 构造 execution principal）→
// operatorFrom 输出。operator_execution_test.go 直接手工构造 principal，
// 证明不了 mint 侧是否真的把调用用户带进链路——本测试补上这一环。
//
// 函数仓储用接口内嵌桩（ClientInvoke 同步路径只触达下列五个方法，其余
// 方法不被调用）。executor 桩捕获注入 env（TW_EXECUTION_TOKEN 即函数容器
// 回访 Server API 用的凭证原值）。
type fullpathFnRepo struct {
	domainfunctions.FunctionRepo
	fn  *domainfunctions.Function
	dep *domainfunctions.Deployment
}

func (r *fullpathFnRepo) GetFunction(_ context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	if r.fn == nil || r.fn.ProjectID != projectID || r.fn.ID != functionID {
		return nil, nil
	}
	return r.fn, nil
}

func (r *fullpathFnRepo) ListDeployments(context.Context, string, string) ([]domainfunctions.Deployment, error) {
	return []domainfunctions.Deployment{*r.dep}, nil
}

func (r *fullpathFnRepo) GetVariables(context.Context, string, string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *fullpathFnRepo) CreateExecution(_ context.Context, e *domainfunctions.ExecutionRecord) error {
	e.ID = "exe_fullpath"
	return nil
}

func (r *fullpathFnRepo) UpdateExecution(context.Context, *domainfunctions.ExecutionRecord) error {
	return nil
}

func (r *fullpathFnRepo) CountClientInvocations(context.Context, string, string, string, time.Time) (int, error) {
	return 0, nil
}

type fullpathExecutor struct {
	captured domainfunctions.Execution
	// onExecute 在执行进行中回调（真实时序：函数容器是在执行期间拿 env 里的
	// token 回访 Server API；执行结束 defer 即吊销，返回后 Validate 必 401）。
	onExecute func(execToken string)
}

func (m *fullpathExecutor) Build(context.Context, string, string, string) error { return nil }
func (m *fullpathExecutor) Execute(_ context.Context, e domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	m.captured = e
	if m.onExecute != nil {
		m.onExecute(e.Env["TW_EXECUTION_TOKEN"])
	}
	return &domainfunctions.ExecutionResult{StatusCode: 0}, nil
}
func (m *fullpathExecutor) RemoveImage(context.Context, string, string) error { return nil }

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
		ID: "fn_full", ProjectID: "p1", Name: "fn", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		ClientCallable: true, ClientPerUserLimit: 5, ClientLimitWindow: domainfunctions.ClientLimitWindowDay,
		DeclaredScopes: []string{"assets:write"},
	}
	executor := &fullpathExecutor{}
	validator := infraauth.NewValidatorWithOneTimeTokens(
		&config.AppConfig{Security: &config.Security{Jwt: &config.Security_Jwt{Secret: "test-secret"}}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, tokenSvc,
	)
	var (
		execToken  string
		operatorOk bool
		snap       domainassets.OperatorSnapshot
	)
	executor.onExecute = func(token string) {
		execToken = token
		// ② Validate：函数容器回访 Server API 时真 Validator 构造 execution
		// principal——invoking_user 必须穿透 Redis 投影回来。
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
	fnUC := appfunctions.NewFunctionsWithUsage(
		&config.AppConfig{}, executor, &fullpathFnRepo{fn: fn,
			dep: &domainfunctions.Deployment{ID: "dep_1", Status: domainfunctions.DeploymentStatusReady}},
		fullpathQueue{}, nil, nil, appfunctions.Semaphores{}, tokenSvc, nil,
	)

	// ① mint：端用户经客户端调用面触发执行（真实 token 服务落 Redis 投影）。
	userCtx := contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorID:   "user-1",
		ActorKind: domainshared.ActorKindEndUser,
		UserID:    "user-1",
		ProjectID: "p1",
	})
	_, err := fnUC.ClientInvoke(userCtx, appfunctions.ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_full"})
	require.NoError(t, err)

	require.True(t, operatorOk, "执行中回调必须已走完 Validate → operatorFrom 断言")
	require.NotEmpty(t, execToken, "执行身份 token 应注入函数容器 env")

	// 执行结束即主动吊销：ClientInvoke 返回（defer 已 DEL）后同 token 二次
	// 校验 401——「常驻的是容器不是凭证」。
	_, err = validator.ValidateCredential(context.Background(), execToken, domainshared.CredentialTypeExecution)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
