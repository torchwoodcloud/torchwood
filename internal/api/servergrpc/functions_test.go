package servergrpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubRepo 是最小 FunctionRepo 桩（仅覆盖测试路径；v4 repo 面无部署/变量）。
type stubRepo struct {
	fn *domainfunctions.Function
}

func (r *stubRepo) CreateFunction(_ context.Context, fn *domainfunctions.Function) error {
	r.fn = fn
	return nil
}
func (r *stubRepo) GetFunction(_ context.Context, _, _ string) (*domainfunctions.Function, error) {
	return r.fn, nil
}
func (r *stubRepo) ListFunctions(context.Context, string) ([]domainfunctions.Function, error) {
	if r.fn == nil {
		return nil, nil
	}
	return []domainfunctions.Function{*r.fn}, nil
}
func (r *stubRepo) UpdateFunction(_ context.Context, fn *domainfunctions.Function) error {
	r.fn = fn
	return nil
}
func (r *stubRepo) DeleteFunction(context.Context, string, string) error { return nil }

func (r *stubRepo) CreateInvocation(context.Context, *domainfunctions.InvocationRecord) error {
	return nil
}
func (r *stubRepo) GetInvocation(context.Context, string, string, string) (*domainfunctions.InvocationRecord, error) {
	return nil, nil
}
func (r *stubRepo) ListInvocations(context.Context, string, string, int, int, domainfunctions.InvocationListFilter) ([]domainfunctions.InvocationRecord, int, error) {
	return nil, 0, nil
}
func (r *stubRepo) UpdateInvocation(context.Context, *domainfunctions.InvocationRecord) error {
	return nil
}
func (r *stubRepo) RecoverOrphanInvocationsInProject(context.Context, string, time.Time, int) (int64, error) {
	return 0, nil
}
func (r *stubRepo) PruneOldInvocationsInProject(context.Context, string, string, int) error {
	return nil
}
func (r *stubRepo) PruneTriggerInvocationsInProject(context.Context, string, string, time.Time) error {
	return nil
}
func (r *stubRepo) GetInvocationByIdempotencyKey(context.Context, string, string, string, string) (*domainfunctions.InvocationRecord, error) {
	return nil, nil
}
func (r *stubRepo) CountClientInvocations(context.Context, string, string, string, time.Time) (int, error) {
	return 0, nil
}
func (r *stubRepo) TransitionInvocationStatus(context.Context, string, string, string, string, string) (bool, error) {
	return false, nil
}
func (r *stubRepo) FailInvocationIfActive(context.Context, string, string, string, string) error {
	return nil
}

type stubInvoker struct{}

func (s *stubInvoker) Invoke(context.Context, domainfunctions.Invocation) (*domainfunctions.InvocationResult, error) {
	return &domainfunctions.InvocationResult{Response: "ok"}, nil
}

type stubNotifier struct{}

func (s *stubNotifier) Notify(context.Context, domainfunctions.Notification) (domainfunctions.NotificationResult, error) {
	return domainfunctions.NotificationResult{}, nil
}

type stubQueue struct{}

func (s *stubQueue) Enqueue(context.Context, string, []byte) error { return nil }
func (s *stubQueue) Dequeue(context.Context, string, time.Duration) ([]byte, string, error) {
	return nil, "", nil
}
func (s *stubQueue) Ack(context.Context, string, string) error { return nil }
func (s *stubQueue) Trim(context.Context, string, int64) error { return nil }

func newTestService(repo *stubRepo) *FunctionsService {
	// jwt.secret 承载 target 密钥加密材料（EncryptionSecret 回退路径）。
	cfg := &config.AppConfig{Security: &config.Security{Jwt: &config.Security_Jwt{Secret: "test-jwt-secret-0123456789abcdef"}}}
	uc := appfunctions.NewFunctions(cfg, &stubInvoker{}, &stubNotifier{}, repo, &stubQueue{}, nil, nil, nil, nil, nil)
	return NewFunctionsService(uc)
}

// principalCtx 返回携带平台 admin principal 的上下文（G2-1 后 functions
// 写方法 use-case 层要求平台 admin，handler 层测试需注入真实凭证形态）。
func principalCtx(projectID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:         "admin-1",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: true,
		UserID:          "admin-1",
		ProjectID:       projectID,
	})
}

func TestFunctionsService_MissingProjectID(t *testing.T) {
	// 无 use-case 也能验证 projectID 校验（在 use-case 访问之前失败）。
	s := NewFunctionsService(nil)
	ctx := context.Background()

	_, err := s.ListFunctions(ctx, &sharedv1.ListRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	_, err = s.GetFunction(ctx, &serverv1.GetFunctionRequest{FunctionId: "f1"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	_, err = s.CreateFunction(ctx, &serverv1.CreateFunctionRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	_, err = s.InvokeFunction(ctx, &serverv1.InvokeFunctionRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestFunctionsService_EmptyProjectIDRejected(t *testing.T) {
	s := NewFunctionsService(nil)
	_, err := s.ListFunctions(principalCtx(""), &sharedv1.ListRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestFunctionsService_CreateFunctionHappyPath(t *testing.T) {
	repo := &stubRepo{}
	s := newTestService(repo)

	timeout := int32(15)
	resp, err := s.CreateFunction(principalCtx("p1"), &serverv1.CreateFunctionRequest{
		Id: "fn_1", Name: "hello", TimeoutSeconds: &timeout, TargetUrl: "https://fn.example.com",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, "fn_1", resp.GetFunction().GetId())
	require.Equal(t, "p1", resp.GetFunction().GetProjectId())
	// target 密钥一次性回显 + 指纹投影（v4 §3 实施勘误）。
	require.NotEmpty(t, resp.GetTargetSecret())
	require.NotEmpty(t, resp.GetFunction().GetTarget().GetSecretFingerprint())

	list, err := s.ListFunctions(principalCtx("p1"), &sharedv1.ListRequest{})
	require.NoError(t, err)
	require.Len(t, list.Functions, 1)
}

func TestFunctionsService_CreateFunctionTargetRequired(t *testing.T) {
	s := newTestService(&stubRepo{})
	_, err := s.CreateFunction(principalCtx("p1"), &serverv1.CreateFunctionRequest{
		Id: "fn_1", Name: "hello", TargetUrl: "http://fn.example.com",
	})
	// 非 dev 环境 http target 拒绝（v4 §3 https-or-dev）。
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "https")
}

func TestFunctionsService_GetFunctionNotFound(t *testing.T) {
	s := newTestService(&stubRepo{})
	_, err := s.GetFunction(principalCtx("p1"), &serverv1.GetFunctionRequest{FunctionId: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestFunctionsService_UpdateFunctionTarget UpdateFunction.target_url 接线：
// url 校验（https-or-dev）在 app 层；合法变更落 repo 且不改密钥（轮换走
// RotateFunctionSecret）。
func TestFunctionsService_UpdateFunctionTarget(t *testing.T) {
	repo := &stubRepo{}
	s := newTestService(repo)

	resp, err := s.CreateFunction(principalCtx("p1"), &serverv1.CreateFunctionRequest{
		Id: "fn_t", Name: "f", TargetUrl: "https://old.example.com",
	})
	require.NoError(t, err)
	oldFingerprint := resp.GetFunction().GetTarget().GetSecretFingerprint()

	// 非法 url 拒绝（不落库）。
	bad := "ftp://fn.example.com"
	_, err = s.UpdateFunction(principalCtx("p1"), &serverv1.UpdateFunctionRequest{
		FunctionId: "fn_t", TargetUrl: &bad,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 合法变更落库。
	good := "https://new.example.com"
	fn, err := s.UpdateFunction(principalCtx("p1"), &serverv1.UpdateFunctionRequest{
		FunctionId: "fn_t", TargetUrl: &good,
	})
	require.NoError(t, err)
	require.Equal(t, "https://new.example.com", fn.GetTarget().GetUrl())
	require.Equal(t, oldFingerprint, repo.fn.TargetSecretFingerprint, "url 更新不触碰密钥")
}

// TestFunctionsService_RotateFunctionSecret 轮换接线：新明文一次性回显 +
// 指纹更新 + 旧钥进宽限窗列。
func TestFunctionsService_RotateFunctionSecret(t *testing.T) {
	repo := &stubRepo{}
	s := newTestService(repo)

	resp, err := s.CreateFunction(principalCtx("p1"), &serverv1.CreateFunctionRequest{
		Id: "fn_r", Name: "f", TargetUrl: "https://fn.example.com",
	})
	require.NoError(t, err)
	oldEnc := repo.fn.TargetSecretEnc
	oldFingerprint := resp.GetFunction().GetTarget().GetSecretFingerprint()

	sec, err := s.RotateFunctionSecret(principalCtx("p1"), &serverv1.RotateFunctionSecretRequest{FunctionId: "fn_r"})
	require.NoError(t, err)
	require.NotEmpty(t, sec.GetSecret())
	require.NotEqual(t, oldFingerprint, sec.GetSecretFingerprint())
	// 双钥宽限窗：新钥入主列、旧钥移 previous、轮换时刻置位。
	require.NotEqual(t, oldEnc, repo.fn.TargetSecretEnc)
	require.Equal(t, oldEnc, repo.fn.TargetPreviousSecretEnc)
	require.False(t, repo.fn.TargetSecretRotatedAt.IsZero())
}
