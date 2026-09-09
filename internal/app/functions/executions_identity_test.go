package functions

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// fakeExecTokens 记录铸造/吊销调用（执行身份 P0 的 app 层集成断言）。
type fakeExecTokens struct {
	minted  []domainfunctions.ExecutionTokenInfo
	ttls    []time.Duration
	tokens  []string
	revoked []string
	mintErr error
	mintSeq int
}

func (f *fakeExecTokens) Mint(_ context.Context, info domainfunctions.ExecutionTokenInfo, ttl time.Duration) (string, error) {
	if f.mintErr != nil {
		return "", f.mintErr
	}
	f.minted = append(f.minted, info)
	f.ttls = append(f.ttls, ttl)
	f.mintSeq++
	token := "twx_test-" + string(rune('a'+f.mintSeq))
	f.tokens = append(f.tokens, token)
	return token, nil
}

func (f *fakeExecTokens) Validate(_ context.Context, token string) (*domainfunctions.ExecutionTokenInfo, error) {
	for i, t := range f.tokens {
		if t == token && i < len(f.minted) {
			return &f.minted[i], nil
		}
	}
	return nil, nil
}

func (f *fakeExecTokens) Revoke(_ context.Context, token string) error {
	f.revoked = append(f.revoked, token)
	return nil
}

// P0 执行身份：同步路径铸造 token、注入 env（TW_EXECUTION_TOKEN +
// TW_API_BASE_URL）并在执行结束后主动吊销（TTL 只是崩溃兜底）。
func TestExecutionIdentity_SyncMintsInjectsAndRevokes(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	fn.DeclaredScopes = []string{"assets:write"}
	require.NoError(t, repo.UpdateFunction(context.Background(), fn))

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Stdout: "ok"}, nil)
	tokens := &fakeExecTokens{}
	cfg := &config.AppConfig{Functions: &config.Functions{Execution: &config.Functions_Execution{ApiBaseUrl: "https://api.example.com"}}}
	uc := NewFunctions(cfg, executor, repo, newMockQueue())
	uc.execTokens = tokens

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, rec.Status)

	// 铸造：身份三元组 + declared scopes + TTL = 超时 + 60s。
	require.Len(t, tokens.minted, 1)
	require.Equal(t, "p1", tokens.minted[0].ProjectID)
	require.Equal(t, "fn_1", tokens.minted[0].FunctionID)
	require.Equal(t, rec.ID, tokens.minted[0].ExecutionID)
	require.Equal(t, []string{"assets:write"}, tokens.minted[0].Scopes)
	// Server 面触发：InvokingUserID 为空是设计内语义（§4.2）。
	require.Empty(t, tokens.minted[0].InvokingUserID)
	require.Len(t, tokens.ttls, 1)
	require.Equal(t, 15*time.Second+executionTokenGrace, tokens.ttls[0])

	// 注入：executor 收到的 env 带 token 与 base URL。
	require.Len(t, executor.calls, 1)
	require.Equal(t, tokens.tokens[0], executor.calls[0].Env[twExecutionTokenEnv])
	require.Equal(t, "https://api.example.com", executor.calls[0].Env[twAPIBaseURLEnv])

	// 主动吊销：执行结束（成功路径）即 DEL，不等 TTL。
	require.Equal(t, []string{tokens.tokens[0]}, tokens.revoked)
}

// 执行失败路径同样吊销（defer 覆盖成功/失败/panic）。
func TestExecutionIdentity_FailurePathRevokes(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	executor := newMockExecutor(nil, context.DeadlineExceeded)
	tokens := &fakeExecTokens{}
	uc := newTestUC(executor, repo, newMockQueue())
	uc.execTokens = tokens

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Error(t, err)
	require.Len(t, tokens.minted, 1)
	require.Equal(t, []string{tokens.tokens[0]}, tokens.revoked)
}

// 铸造失败 best-effort：不阻断执行，env 不注入 token，也无吊销可做。
func TestExecutionIdentity_MintFailureDegradesGracefully(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
	tokens := &fakeExecTokens{mintErr: context.DeadlineExceeded}
	uc := newTestUC(executor, repo, newMockQueue())
	uc.execTokens = tokens

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, rec.Status)
	require.NotContains(t, executor.calls[0].Env, twExecutionTokenEnv)
	require.Empty(t, tokens.revoked)
}

// api_base_url 未配置（或 token 服务未装配）时不注入对应 env。
func TestExecutionIdentity_NoConfigNoInjection(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
	tokens := &fakeExecTokens{}
	uc := newTestUC(executor, repo, newMockQueue())
	uc.execTokens = tokens

	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)
	require.NotContains(t, executor.calls[0].Env, twAPIBaseURLEnv)
	require.Contains(t, executor.calls[0].Env, twExecutionTokenEnv)
}

// 客户端调用面（P2）：铸造的 token info 携带调用用户（InvokingUserID）——
// 漏填会让 execution principal / 账本 operator 的 user 维度全链路落空
// （uid 只剩 TW_DATA 客户端自报，可伪造）。触发器路径不经过本入口，不受影响。
func TestExecutionIdentity_ClientInvokeMintsInvokingUser(t *testing.T) {
	repo := newMockRepo()
	fn := seedClientCallableFunction(t, repo, "p1", "fn_1")
	fn.DeclaredScopes = []string{"assets:write"}
	require.NoError(t, repo.UpdateFunction(context.Background(), fn))

	executor := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0}, nil)
	tokens := &fakeExecTokens{}
	uc := newTestUC(executor, repo, newMockQueue())
	uc.execTokens = tokens
	uc.clientQuota = &fakeQuotaLimiter{}

	res, err := uc.ClientInvoke(endUserCtx(), ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.NoError(t, err)

	// 铸造：invoking_user = 调用者 uid（principal 注入，非请求体自报）。
	require.Len(t, tokens.minted, 1)
	require.Equal(t, "p1", tokens.minted[0].ProjectID)
	require.Equal(t, "fn_1", tokens.minted[0].FunctionID)
	require.Equal(t, res.Record.ID, tokens.minted[0].ExecutionID)
	require.Equal(t, "user-1", tokens.minted[0].InvokingUserID)
	require.Equal(t, []string{"assets:write"}, tokens.minted[0].Scopes)

	// 注入与吊销语义不变。
	require.Equal(t, tokens.tokens[0], executor.calls[0].Env[twExecutionTokenEnv])
	require.Equal(t, []string{tokens.tokens[0]}, tokens.revoked)
}
