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

// stubRepo 是最小 FunctionRepo 桩（仅覆盖测试路径）。
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
func (r *stubRepo) DeleteFunction(context.Context, string, string) error                { return nil }
func (r *stubRepo) CreateDeployment(context.Context, *domainfunctions.Deployment) error { return nil }
func (r *stubRepo) GetDeployment(context.Context, string, string, string) (*domainfunctions.Deployment, error) {
	return nil, nil
}
func (r *stubRepo) ListDeployments(context.Context, string, string) ([]domainfunctions.Deployment, error) {
	return nil, nil
}
func (r *stubRepo) UpdateDeployment(context.Context, *domainfunctions.Deployment) error { return nil }

func (r *stubRepo) ActivateDeployment(context.Context, *domainfunctions.Deployment) error { return nil }
func (r *stubRepo) DeleteDeployment(context.Context, string, string, string) error        { return nil }
func (r *stubRepo) SetVariables(context.Context, string, string, map[string]string) error {
	return nil
}
func (r *stubRepo) GetVariables(context.Context, string, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *stubRepo) CreateExecution(context.Context, *domainfunctions.ExecutionRecord) error {
	return nil
}
func (r *stubRepo) GetExecution(context.Context, string, string, string) (*domainfunctions.ExecutionRecord, error) {
	return nil, nil
}
func (r *stubRepo) ListExecutions(context.Context, string, string, int) ([]domainfunctions.ExecutionRecord, error) {
	return nil, nil
}
func (r *stubRepo) UpdateExecution(context.Context, *domainfunctions.ExecutionRecord) error {
	return nil
}
func (r *stubRepo) RecoverOrphanExecutionsInProject(context.Context, string, time.Time, int) (int64, error) {
	return 0, nil
}
func (r *stubRepo) PruneOldExecutionsInProject(context.Context, string, string, int) error {
	return nil
}
func (r *stubRepo) PruneTriggerExecutionsInProject(context.Context, string, string, time.Time) error {
	return nil
}
func (r *stubRepo) GetExecutionByIdempotencyKey(context.Context, string, string, string, string) (*domainfunctions.ExecutionRecord, error) {
	return nil, nil
}
func (r *stubRepo) CountClientInvocations(context.Context, string, string, string, time.Time) (int, error) {
	return 0, nil
}
func (r *stubRepo) TransitionExecutionStatus(context.Context, string, string, string, string, string) (bool, error) {
	return false, nil
}
func (r *stubRepo) FailExecutionIfActive(context.Context, string, string, string, string) error {
	return nil
}

type stubExecutor struct{}

func (s *stubExecutor) Build(context.Context, string, string, string) error { return nil }
func (s *stubExecutor) Execute(context.Context, domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	return &domainfunctions.ExecutionResult{StatusCode: 0, Stdout: "ok"}, nil
}
func (s *stubExecutor) RemoveImage(context.Context, string, string) error { return nil }

type stubQueue struct{}

func (s *stubQueue) Enqueue(context.Context, string, []byte) error { return nil }
func (s *stubQueue) Dequeue(context.Context, string, time.Duration) ([]byte, string, error) {
	return nil, "", nil
}
func (s *stubQueue) Ack(context.Context, string, string) error { return nil }
func (s *stubQueue) Trim(context.Context, string, int64) error { return nil }

func newTestService(repo *stubRepo) *FunctionsService {
	uc := appfunctions.NewFunctions(&config.AppConfig{}, &stubExecutor{}, repo, &stubQueue{})
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

	_, err = s.ListRuntimes(ctx, &sharedv1.Empty{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestFunctionsService_EmptyProjectIDRejected(t *testing.T) {
	s := NewFunctionsService(nil)
	_, err := s.ListFunctions(principalCtx(""), &sharedv1.ListRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestFunctionsService_CreateFunctionValidatesBeforeRepo(t *testing.T) {
	s := newTestService(&stubRepo{})

	// 非法 runtime → InvalidArgument 透传（不落库）。
	timeout := int32(15)
	_, err := s.CreateFunction(principalCtx("p1"), &serverv1.CreateFunctionRequest{
		Id: "fn_1", Name: "f", Runtime: "bogus", TimeoutSeconds: &timeout,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "unsupported runtime")
}

func TestFunctionsService_CreateFunctionHappyPath(t *testing.T) {
	repo := &stubRepo{}
	s := newTestService(repo)

	timeout := int32(15)
	spec := "shared-1x"
	fn, err := s.CreateFunction(principalCtx("p1"), &serverv1.CreateFunctionRequest{
		Id: "fn_1", Name: "hello", Runtime: "node-18.0", TimeoutSeconds: &timeout, Spec: &spec,
	})
	require.NoError(t, err)
	require.NotNil(t, fn)
	require.Equal(t, "fn_1", fn.Id)
	require.Equal(t, "p1", fn.ProjectId)

	list, err := s.ListFunctions(principalCtx("p1"), &sharedv1.ListRequest{})
	require.NoError(t, err)
	require.Len(t, list.Functions, 1)
}

func TestFunctionsService_GetFunctionNotFound(t *testing.T) {
	s := newTestService(&stubRepo{})
	_, err := s.GetFunction(principalCtx("p1"), &serverv1.GetFunctionRequest{FunctionId: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}
