package client

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// recorder 记录 fake 服务收到的 metadata 与关键请求，供断言使用。
type recorder struct {
	mu              sync.Mutex
	lastAuth        metadata.MD
	lastGetDocument *clientv1.GetDocumentRequest
	upserts         []*clientv1.UpsertDocumentRequest
}

func (r *recorder) record(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastAuth = md
}

func (r *recorder) auth(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if vals := r.lastAuth.Get(key); len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// fakeGroups 实现 Client API Groups 服务。
type fakeGroups struct {
	clientv1.UnimplementedGroupsServiceServer
	rec *recorder
}

func (s *fakeGroups) CreateGroup(ctx context.Context, req *clientv1.CreateGroupRequest) (*clientv1.Group, error) {
	s.rec.record(ctx)
	return &clientv1.Group{Id: "group-1", Name: req.Name}, nil
}

func (s *fakeGroups) GetGroup(ctx context.Context, req *clientv1.GetGroupRequest) (*clientv1.Group, error) {
	s.rec.record(ctx)
	return &clientv1.Group{Id: req.Id, Name: "Group One"}, nil
}

func (s *fakeGroups) ListGroups(ctx context.Context, _ *sharedv1.ListRequest) (*clientv1.ListGroupsResponse, error) {
	s.rec.record(ctx)
	return &clientv1.ListGroupsResponse{Groups: []*clientv1.Group{{Id: "group-1", Name: "Group One"}}}, nil
}

func (s *fakeGroups) CreateMembership(ctx context.Context, req *clientv1.CreateMembershipRequest) (*clientv1.Membership, error) {
	s.rec.record(ctx)
	return &clientv1.Membership{Id: "mem-1", GroupId: req.GroupId, Email: req.Email, Roles: req.Roles}, nil
}

func (s *fakeGroups) ListMemberships(ctx context.Context, req *clientv1.ListMembershipsRequest) (*clientv1.ListMembershipsResponse, error) {
	s.rec.record(ctx)
	return &clientv1.ListMembershipsResponse{Memberships: []*clientv1.Membership{{Id: "mem-1", GroupId: req.GroupId}}}, nil
}

func (s *fakeGroups) UpdateMembershipStatus(ctx context.Context, req *clientv1.UpdateMembershipStatusRequest) (*clientv1.Membership, error) {
	s.rec.record(ctx)
	return &clientv1.Membership{Id: req.MembershipId, GroupId: req.GroupId, Status: req.Status}, nil
}

func (s *fakeGroups) DeleteMembership(ctx context.Context, _ *clientv1.GetMembershipRequest) (*sharedv1.Empty, error) {
	s.rec.record(ctx)
	return &sharedv1.Empty{}, nil
}

func (s *fakeGroups) DeleteGroup(ctx context.Context, _ *clientv1.GetGroupRequest) (*sharedv1.Empty, error) {
	s.rec.record(ctx)
	return &sharedv1.Empty{}, nil
}

// fakeDatabases 实现 Client API Databases 服务。
type fakeDatabases struct {
	clientv1.UnimplementedDatabasesServiceServer
	rec *recorder
}

func (s *fakeDatabases) CreateDocument(ctx context.Context, req *clientv1.CreateDocumentRequest) (*sharedv1.Document, error) {
	s.rec.record(ctx)
	return &sharedv1.Document{Id: req.DocumentId, Data: req.Data, Permissions: req.Permissions}, nil
}

func (s *fakeDatabases) GetDocument(ctx context.Context, req *clientv1.GetDocumentRequest) (*sharedv1.Document, error) {
	s.rec.record(ctx)
	s.rec.mu.Lock()
	s.rec.lastGetDocument = req
	s.rec.mu.Unlock()
	return &sharedv1.Document{Id: req.DocumentId}, nil
}

func (s *fakeDatabases) UpsertDocument(ctx context.Context, req *clientv1.UpsertDocumentRequest) (*sharedv1.Document, error) {
	s.rec.record(ctx)
	s.rec.mu.Lock()
	s.rec.upserts = append(s.rec.upserts, req)
	s.rec.mu.Unlock()
	return &sharedv1.Document{Id: req.DocumentId, Data: req.Data}, nil
}

func (s *fakeDatabases) UpdateDocument(ctx context.Context, req *clientv1.UpdateDocumentRequest) (*sharedv1.Document, error) {
	s.rec.record(ctx)
	return &sharedv1.Document{Id: req.DocumentId, Data: req.Data}, nil
}

func (s *fakeDatabases) DeleteDocument(ctx context.Context, _ *clientv1.DeleteDocumentRequest) (*sharedv1.Empty, error) {
	s.rec.record(ctx)
	return &sharedv1.Empty{}, nil
}

func (s *fakeDatabases) ListDocuments(ctx context.Context, _ *clientv1.ListDocumentsRequest) (*clientv1.ListDocumentsResponse, error) {
	s.rec.record(ctx)
	return &clientv1.ListDocumentsResponse{Documents: []*sharedv1.Document{{Id: "d1"}}}, nil
}

func (s *fakeDatabases) CountDocuments(ctx context.Context, _ *clientv1.CountDocumentsRequest) (*clientv1.CountDocumentsResponse, error) {
	s.rec.record(ctx)
	return &clientv1.CountDocumentsResponse{Count: 7}, nil
}

// newFullBufconn 启动注册了 Account/Groups/Databases fake 的 bufconn gRPC 服务。
func newFullBufconn(t *testing.T) (*bufconn.Listener, *recorder) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	rec := &recorder{}
	srv := grpc.NewServer()
	clientv1.RegisterAccountServiceServer(srv, &fakeAccount{})
	clientv1.RegisterGroupsServiceServer(srv, &fakeGroups{rec: rec})
	clientv1.RegisterDatabasesServiceServer(srv, &fakeDatabases{rec: rec})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis, rec
}

// newFullClient 返回连接 Account/Groups/Databases fake 的 Client API 客户端。
func newFullClient(t *testing.T, opts ...Option) (*Client, *recorder) {
	t.Helper()
	lis, rec := newFullBufconn(t)
	opts = append(opts, WithDialOptions(grpc.WithContextDialer(
		func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })))
	c, err := New("passthrough:///bufconn", opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, rec
}

func TestClientGroups_CreateAndList(t *testing.T) {
	c, _ := newFullClient(t, WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}))
	ctx := context.Background()

	group, err := c.Groups.CreateGroup(ctx, "Group One")
	require.NoError(t, err)
	require.Equal(t, "group-1", group.Id)
	require.Equal(t, "Group One", group.Name)

	got, err := c.Groups.GetGroup(ctx, "group-1")
	require.NoError(t, err)
	require.Equal(t, "Group One", got.Name)

	list, err := c.Groups.ListGroups(ctx)
	require.NoError(t, err)
	require.Len(t, list.Groups, 1)
}

func TestClientGroups_Memberships(t *testing.T) {
	c, _ := newFullClient(t, WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}))
	ctx := context.Background()

	mem, err := c.Groups.CreateMembership(ctx, "group-1", "bob@example.com", "Bob", []string{"member"})
	require.NoError(t, err)
	require.Equal(t, "group-1", mem.GroupId)
	require.Equal(t, []string{"member"}, mem.Roles)

	listed, err := c.Groups.ListMemberships(ctx, "group-1")
	require.NoError(t, err)
	require.Len(t, listed.Memberships, 1)

	updated, err := c.Groups.UpdateMembershipStatus(ctx, "group-1", "mem-1", "active")
	require.NoError(t, err)
	require.Equal(t, "active", updated.Status)

	require.NoError(t, c.Groups.DeleteMembership(ctx, "group-1", "mem-1"))
}

// Round3 H4-3：Go Client 补齐 DeleteGroup（对齐 TS / proto）。
func TestClientGroups_DeleteGroup(t *testing.T) {
	c, rec := newFullClient(t, WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}))
	ctx := context.Background()

	require.NoError(t, c.Groups.DeleteGroup(ctx, "group-1"))
	require.Equal(t, "Bearer jwt-1", rec.auth("authorization"), "DeleteGroup 必须携带 Bearer token")
}

func TestClientDatabases_DocumentCRUD(t *testing.T) {
	c, _ := newFullClient(t, WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}), WithDatabaseID("app"))
	ctx := context.Background()
	docs := c.Databases

	created, err := docs.CreateDocument(ctx, "members", "m1",
		map[string]any{"channel_id": "ch1", "user_id": "u1", "last_read_seq": 10},
		[]string{"read:user:u1"})
	require.NoError(t, err)
	require.Equal(t, "m1", created.Id)
	require.Equal(t, float64(10), created.Data.GetFields()["last_read_seq"].GetNumberValue())

	got, err := docs.GetDocument(ctx, "members", "m1")
	require.NoError(t, err)
	require.Equal(t, "m1", got.Id)

	updated, err := docs.UpdateDocument(ctx, "members", "m1",
		map[string]any{"last_read_seq": 42}, nil, nil, 1)
	require.NoError(t, err)
	require.Equal(t, "m1", updated.Id)

	require.NoError(t, docs.DeleteDocument(ctx, "members", "m1", 2))
}

func TestClientDatabases_UpsertForwardsConflictColumns(t *testing.T) {
	c, rec := newFullClient(t, WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}), WithDatabaseID("app"))
	ctx := context.Background()

	doc, err := c.Databases.UpsertDocument(ctx, "members", "m1",
		map[string]any{"channel_id": "ch1", "user_id": "u1", "last_read_seq": 42},
		[]string{"channel_id", "user_id"},
		[]string{"read:user:u1"})
	require.NoError(t, err)
	require.Equal(t, "m1", doc.Id)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.upserts, 1)
	require.Equal(t, []string{"channel_id", "user_id"}, rec.upserts[0].ConflictColumns)
	require.Equal(t, []string{"read:user:u1"}, rec.upserts[0].Permissions)
	require.Equal(t, "app", rec.upserts[0].DatabaseId)
}

func TestClientDatabases_ListAndCount(t *testing.T) {
	c, _ := newFullClient(t, WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}), WithDatabaseID("app"))
	ctx := context.Background()

	docs, next, err := c.Databases.ListDocuments(ctx, "members",
		&sharedv1.Query{Filter: eqFilter("channel_id", "ch1"), PageSize: 20})
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, "", next)

	count, err := c.Databases.CountDocuments(ctx, "members",
		&sharedv1.Query{Filter: eqFilter("channel_id", "ch1")})
	require.NoError(t, err)
	require.Equal(t, int64(7), count)
}

// eqFilter 是测试用的 equal 过滤构造捷径。
func eqFilter(attr string, values ...string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: &sharedv1.Comparison{
		Attribute: attr, Values: values,
	}}}
}

func TestClientDatabases_UseDatabaseOverride(t *testing.T) {
	c, rec := newFullClient(t, WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}), WithDatabaseID("app"))
	ctx := context.Background()

	_, err := c.UseDatabase("other").GetDocument(ctx, "members", "m1")
	require.NoError(t, err)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.NotNil(t, rec.lastGetDocument)
	require.Equal(t, "other", rec.lastGetDocument.DatabaseId)
	require.Equal(t, "app", c.Databases.db)
}
