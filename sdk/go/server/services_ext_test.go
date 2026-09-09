package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func strptr(s string) *string { return &s }

// fakeExt 实现 Projects/Storage/Functions/OAuthProviders fake 服务。
type fakeExt struct {
	serverv1.UnimplementedProjectsServiceServer
	serverv1.UnimplementedStorageServiceServer
	serverv1.UnimplementedFunctionsServiceServer
	serverv1.UnimplementedOAuthProvidersServiceServer
}

func (f *fakeExt) CreateProject(_ context.Context, req *serverv1.CreateProjectRequest) (*serverv1.Project, error) {
	return &serverv1.Project{Id: "p1", Name: req.Name}, nil
}

func (f *fakeExt) ListProjects(_ context.Context, req *sharedv1.ListRequest) (*serverv1.ListProjectsResponse, error) {
	return &serverv1.ListProjectsResponse{Projects: []*serverv1.Project{{Id: "p1"}}}, nil
}

func (f *fakeExt) GetProject(_ context.Context, req *serverv1.GetProjectRequest) (*serverv1.Project, error) {
	return &serverv1.Project{Id: req.Id}, nil
}

func (f *fakeExt) UpdateProject(_ context.Context, req *serverv1.UpdateProjectRequest) (*serverv1.Project, error) {
	return &serverv1.Project{Id: req.Id, Name: req.GetName()}, nil
}

func (f *fakeExt) DeleteProject(_ context.Context, req *serverv1.GetProjectRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (f *fakeExt) CreateBucket(_ context.Context, req *serverv1.CreateBucketRequest) (*serverv1.Bucket, error) {
	return &serverv1.Bucket{Id: req.Name, Name: req.Name, Permissions: req.Permissions, Public: req.Public}, nil
}

func (f *fakeExt) ListBuckets(_ context.Context, req *sharedv1.ListRequest) (*serverv1.ListBucketsResponse, error) {
	return &serverv1.ListBucketsResponse{Buckets: []*serverv1.Bucket{{Id: "b1"}}}, nil
}

func (f *fakeExt) GetBucket(_ context.Context, req *serverv1.GetBucketRequest) (*serverv1.Bucket, error) {
	return &serverv1.Bucket{Id: req.Id}, nil
}

func (f *fakeExt) DeleteBucket(_ context.Context, req *serverv1.GetBucketRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (f *fakeExt) UpdateBucket(_ context.Context, req *serverv1.UpdateBucketRequest) (*serverv1.Bucket, error) {
	return &serverv1.Bucket{Id: req.Id, Name: req.GetName(), Public: req.GetPublic()}, nil
}

func (f *fakeExt) CreateFile(_ context.Context, req *serverv1.CreateFileRequest) (*serverv1.File, error) {
	return &serverv1.File{Id: "file-1", BucketId: req.BucketId, Name: req.Name}, nil
}

func (f *fakeExt) ListFiles(_ context.Context, req *serverv1.ListFilesRequest) (*serverv1.ListFilesResponse, error) {
	return &serverv1.ListFilesResponse{Files: []*serverv1.File{{Id: "f1"}}}, nil
}

func (f *fakeExt) GetFile(_ context.Context, req *serverv1.GetFileRequest) (*serverv1.File, error) {
	return &serverv1.File{Id: req.FileId, BucketId: req.BucketId}, nil
}

func (f *fakeExt) DeleteFile(_ context.Context, req *serverv1.GetFileRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (f *fakeExt) UpdateFile(_ context.Context, req *serverv1.UpdateFileRequest) (*serverv1.File, error) {
	return &serverv1.File{Id: req.FileId, BucketId: req.BucketId, Name: req.GetName()}, nil
}

func (f *fakeExt) CreateFileToken(_ context.Context, req *serverv1.CreateFileTokenRequest) (*serverv1.FileToken, error) {
	return &serverv1.FileToken{Token: "tok"}, nil
}

func (f *fakeExt) GetStorageUsage(_ context.Context, _ *serverv1.GetStorageUsageRequest) (*serverv1.StorageUsage, error) {
	return &serverv1.StorageUsage{Buckets: 1, Files: 2, TotalSize: 1024}, nil
}

func (f *fakeExt) ListRuntimes(_ context.Context, _ *sharedv1.Empty) (*serverv1.ListRuntimesResponse, error) {
	return &serverv1.ListRuntimesResponse{Runtimes: []*serverv1.RuntimeInfo{{Id: "nodejs18"}}}, nil
}

func (f *fakeExt) ListSpecifications(_ context.Context, _ *sharedv1.Empty) (*serverv1.ListSpecificationsResponse, error) {
	return &serverv1.ListSpecificationsResponse{Specifications: []*serverv1.SpecificationInfo{{Id: "shared-1x"}}}, nil
}

func (f *fakeExt) CreateFunction(_ context.Context, req *serverv1.CreateFunctionRequest) (*serverv1.Function, error) {
	return &serverv1.Function{Id: req.Id, Name: req.Name, Runtime: req.Runtime}, nil
}

func (f *fakeExt) ListFunctions(_ context.Context, req *sharedv1.ListRequest) (*serverv1.ListFunctionsResponse, error) {
	return &serverv1.ListFunctionsResponse{Functions: []*serverv1.Function{{Id: "fn1"}}}, nil
}

func (f *fakeExt) GetFunction(_ context.Context, req *serverv1.GetFunctionRequest) (*serverv1.Function, error) {
	return &serverv1.Function{Id: req.FunctionId}, nil
}

func (f *fakeExt) UpdateFunction(_ context.Context, req *serverv1.UpdateFunctionRequest) (*serverv1.Function, error) {
	return &serverv1.Function{Id: req.FunctionId, Name: req.GetName()}, nil
}

func (f *fakeExt) DeleteFunction(_ context.Context, req *serverv1.GetFunctionRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (f *fakeExt) CreateDeployment(_ context.Context, req *serverv1.CreateDeploymentRequest) (*serverv1.Deployment, error) {
	return &serverv1.Deployment{FunctionId: req.FunctionId, Size: int64(len(req.Code))}, nil
}

func (f *fakeExt) ListDeployments(_ context.Context, req *serverv1.GetFunctionRequest) (*serverv1.ListDeploymentsResponse, error) {
	return &serverv1.ListDeploymentsResponse{Deployments: []*serverv1.Deployment{{FunctionId: req.FunctionId}}}, nil
}

func (f *fakeExt) GetDeployment(_ context.Context, req *serverv1.GetDeploymentRequest) (*serverv1.Deployment, error) {
	return &serverv1.Deployment{FunctionId: req.FunctionId, Id: req.DeploymentId}, nil
}

func (f *fakeExt) DeleteDeployment(_ context.Context, req *serverv1.GetDeploymentRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (f *fakeExt) SetVariables(_ context.Context, req *serverv1.SetVariablesRequest) (*serverv1.Variables, error) {
	return &serverv1.Variables{Variables: req.Variables}, nil
}

func (f *fakeExt) GetVariables(_ context.Context, req *serverv1.GetFunctionRequest) (*serverv1.Variables, error) {
	return &serverv1.Variables{Variables: []*serverv1.Variable{{Key: "FOO", Value: "bar"}}}, nil
}

func (f *fakeExt) CreateExecution(_ context.Context, req *serverv1.CreateExecutionRequest) (*serverv1.Execution, error) {
	return &serverv1.Execution{Id: "ex-1", FunctionId: req.FunctionId}, nil
}

func (f *fakeExt) ListExecutions(_ context.Context, req *serverv1.GetFunctionRequest) (*serverv1.ListExecutionsResponse, error) {
	return &serverv1.ListExecutionsResponse{Executions: []*serverv1.Execution{{Id: "ex-1", FunctionId: req.FunctionId}}}, nil
}

func (f *fakeExt) GetExecution(_ context.Context, req *serverv1.GetExecutionRequest) (*serverv1.Execution, error) {
	return &serverv1.Execution{Id: req.ExecutionId, FunctionId: req.FunctionId}, nil
}

func (f *fakeExt) ListOAuthProviders(_ context.Context, req *sharedv1.ListRequest) (*serverv1.ListOAuthProvidersResponse, error) {
	return &serverv1.ListOAuthProvidersResponse{OauthProviders: []*serverv1.OAuthProvider{{Provider: "google"}}}, nil
}

func (f *fakeExt) UpsertOAuthProvider(_ context.Context, req *serverv1.UpsertOAuthProviderRequest) (*serverv1.OAuthProvider, error) {
	return &serverv1.OAuthProvider{Provider: req.Provider, Enabled: req.Enabled, ClientId: req.ClientId}, nil
}

func (f *fakeExt) DeleteOAuthProvider(_ context.Context, req *serverv1.DeleteOAuthProviderRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

// newExtBufconn 启动注册了 4 个扩展 fake 服务的 bufconn gRPC 服务。
func newExtBufconn(t *testing.T) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeExt{}
	serverv1.RegisterProjectsServiceServer(srv, fake)
	serverv1.RegisterStorageServiceServer(srv, fake)
	serverv1.RegisterFunctionsServiceServer(srv, fake)
	serverv1.RegisterOAuthProvidersServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis
}

func newExtClient(t *testing.T) *Client {
	t.Helper()
	lis := newExtBufconn(t)
	return newTestClient(t, lis, WithAPIKey("k"))
}

func TestProjectsCreateProject(t *testing.T) {
	c := newExtClient(t)
	ctx := context.Background()

	got, err := c.Projects.CreateProject(ctx, &serverv1.CreateProjectRequest{Name: "Proj"})
	require.NoError(t, err)
	require.Equal(t, "p1", got.Id)
	require.Equal(t, "Proj", got.Name)
}

func TestProjectsDeleteProject(t *testing.T) {
	c := newExtClient(t)
	ctx := context.Background()

	_, err := c.Projects.DeleteProject(ctx, &serverv1.GetProjectRequest{Id: "p1"})
	require.NoError(t, err)
}

func TestStorageCreateBucket(t *testing.T) {
	c := newExtClient(t)
	ctx := context.Background()

	got, err := c.Storage.CreateBucket(ctx, &serverv1.CreateBucketRequest{
		Name:        "assets",
		Permissions: []string{"read:all"},
		Public:      true,
	})
	require.NoError(t, err)
	require.Equal(t, "assets", got.Id)
	require.True(t, got.Public)
	require.Equal(t, []string{"read:all"}, got.Permissions)
}

func TestFunctionsCreateFunction(t *testing.T) {
	c := newExtClient(t)
	ctx := context.Background()

	got, err := c.Functions.CreateFunction(ctx, &serverv1.CreateFunctionRequest{
		Id:      "fn1",
		Name:    "hello",
		Runtime: "nodejs18",
	})
	require.NoError(t, err)
	require.Equal(t, "fn1", got.Id)
	require.Equal(t, "nodejs18", got.Runtime)
}

func TestOAuthProvidersUpsert(t *testing.T) {
	c := newExtClient(t)
	ctx := context.Background()

	got, err := c.OAuthProviders.UpsertOAuthProvider(ctx, &serverv1.UpsertOAuthProviderRequest{
		Provider:     "google",
		Enabled:      true,
		ClientId:     "cid",
		Scopes:       []string{"email"},
		ClientSecret: strptr("sec"),
	})
	require.NoError(t, err)
	require.Equal(t, "google", got.Provider)
	require.True(t, got.Enabled)
}
