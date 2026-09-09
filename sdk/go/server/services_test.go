package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeServices 实现 Server API 的 Health/Users/Groups/Databases fake 服务。
type fakeServices struct {
	rec *recorder
	serverv1.UnimplementedHealthServiceServer
	serverv1.UnimplementedUsersServiceServer
	serverv1.UnimplementedGroupsServiceServer
	serverv1.UnimplementedDatabasesServiceServer
}

func (s *fakeServices) Check(ctx context.Context, _ *serverv1.HealthCheckRequest) (*serverv1.HealthCheckResponse, error) {
	return &serverv1.HealthCheckResponse{Status: "SERVING"}, nil
}

func (s *fakeServices) GetVersion(ctx context.Context, _ *serverv1.GetVersionRequest) (*serverv1.GetVersionResponse, error) {
	return &serverv1.GetVersionResponse{Version: "v0.1.0", Commit: "abc123"}, nil
}

func (s *fakeServices) CreateUser(ctx context.Context, req *serverv1.CreateUserRequest) (*serverv1.User, error) {
	s.rec.mu.Lock()
	s.rec.createdUser = req
	s.rec.mu.Unlock()
	return &serverv1.User{Id: "user-1", Email: req.Email, Name: req.Name, Status: req.Status}, nil
}

func (s *fakeServices) GetUser(ctx context.Context, req *serverv1.GetUserRequest) (*serverv1.User, error) {
	return &serverv1.User{Id: req.Id, Email: "agent-1@agents.local", Name: "Agent One"}, nil
}

func (s *fakeServices) ListUsers(ctx context.Context, _ *sharedv1.ListRequest) (*serverv1.ListUsersResponse, error) {
	return &serverv1.ListUsersResponse{Users: []*serverv1.User{{Id: "user-1", Email: "a@example.com"}}}, nil
}

func (s *fakeServices) UpdateUser(ctx context.Context, req *serverv1.UpdateUserRequest) (*serverv1.User, error) {
	return &serverv1.User{Id: req.Id, Name: deref(req.Name), Status: deref(req.Status)}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *fakeServices) UpdateUserPassword(ctx context.Context, req *serverv1.UpdateUserPasswordRequest) (*serverv1.User, error) {
	if err := s.rec.fail("UpdateUserPassword"); err != nil {
		return nil, err
	}
	s.rec.mu.Lock()
	s.rec.lastUserPassword = req
	s.rec.mu.Unlock()
	return &serverv1.User{Id: req.Id}, nil
}

func (s *fakeServices) DeleteUser(ctx context.Context, _ *serverv1.GetUserRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (s *fakeServices) ListUserSessions(ctx context.Context, _ *serverv1.GetUserRequest) (*serverv1.ListUserSessionsResponse, error) {
	return &serverv1.ListUserSessionsResponse{Sessions: []*serverv1.Session{{Id: "sess-1"}}}, nil
}

func (s *fakeServices) DeleteUserSession(ctx context.Context, _ *serverv1.DeleteUserSessionRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (s *fakeServices) CreateUserToken(ctx context.Context, _ *serverv1.GetUserRequest) (*serverv1.CreateUserTokenResponse, error) {
	return &serverv1.CreateUserTokenResponse{Tokens: &serverv1.TokenBundle{AccessToken: "agent-token"}}, nil
}

func (s *fakeServices) CreateGroup(ctx context.Context, req *serverv1.CreateGroupRequest) (*serverv1.Group, error) {
	return &serverv1.Group{Id: "group-1", Name: req.Name, Permissions: req.Permissions}, nil
}

func (s *fakeServices) GetGroup(ctx context.Context, req *serverv1.GetGroupRequest) (*serverv1.Group, error) {
	return &serverv1.Group{Id: req.Id, Name: "Group One"}, nil
}

func (s *fakeServices) DeleteGroup(ctx context.Context, _ *serverv1.GetGroupRequest) (*sharedv1.Empty, error) {
	if err := s.rec.fail("DeleteGroup"); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *fakeServices) GetGroupPrefs(ctx context.Context, _ *serverv1.GetGroupRequest) (*serverv1.GetGroupPrefsResponse, error) {
	if err := s.rec.fail("GetGroupPrefs"); err != nil {
		return nil, err
	}
	return &serverv1.GetGroupPrefsResponse{Prefs: &structpb.Struct{}}, nil
}

func (s *fakeServices) UpdateGroupPrefs(ctx context.Context, req *serverv1.UpdateGroupPrefsRequest) (*serverv1.GetGroupPrefsResponse, error) {
	if err := s.rec.fail("UpdateGroupPrefs"); err != nil {
		return nil, err
	}
	s.rec.mu.Lock()
	s.rec.lastGroupPrefs = req
	s.rec.mu.Unlock()
	return &serverv1.GetGroupPrefsResponse{Prefs: req.Prefs}, nil
}

func (s *fakeServices) ListGroups(ctx context.Context, _ *sharedv1.ListRequest) (*serverv1.ListGroupsResponse, error) {
	return &serverv1.ListGroupsResponse{Groups: []*serverv1.Group{{Id: "group-1", Name: "Group One"}}}, nil
}

func (s *fakeServices) CreateMembership(ctx context.Context, req *serverv1.CreateMembershipRequest) (*serverv1.Membership, error) {
	return &serverv1.Membership{Id: "mem-1", GroupId: req.GroupId, UserId: req.UserId, Roles: req.Roles, Status: req.Status}, nil
}

func (s *fakeServices) ListMemberships(ctx context.Context, req *serverv1.ListMembershipsRequest) (*serverv1.ListMembershipsResponse, error) {
	return &serverv1.ListMembershipsResponse{Memberships: []*serverv1.Membership{{Id: "mem-1", GroupId: req.GroupId}}}, nil
}

func (s *fakeServices) GetMembership(ctx context.Context, req *serverv1.GetMembershipRequest) (*serverv1.Membership, error) {
	return &serverv1.Membership{Id: req.MembershipId, GroupId: req.GroupId}, nil
}

func (s *fakeServices) UpdateMembership(ctx context.Context, req *serverv1.UpdateMembershipRequest) (*serverv1.Membership, error) {
	return &serverv1.Membership{Id: req.MembershipId, GroupId: req.GroupId, Roles: req.Roles}, nil
}

func (s *fakeServices) UpdateMembershipStatus(ctx context.Context, req *serverv1.UpdateMembershipStatusRequest) (*serverv1.Membership, error) {
	return &serverv1.Membership{Id: req.MembershipId, GroupId: req.GroupId, Status: req.Status}, nil
}

func (s *fakeServices) DeleteMembership(ctx context.Context, _ *serverv1.GetMembershipRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (s *fakeServices) CreateDatabase(ctx context.Context, req *serverv1.CreateDatabaseRequest) (*serverv1.Database, error) {
	return &serverv1.Database{Id: req.Id, Name: req.Name}, nil
}

func (s *fakeServices) GetDatabase(ctx context.Context, req *serverv1.GetDatabaseRequest) (*serverv1.Database, error) {
	return &serverv1.Database{Id: req.Id, Name: req.Id}, nil
}

func (s *fakeServices) ListDatabases(ctx context.Context, _ *sharedv1.ListRequest) (*serverv1.ListDatabasesResponse, error) {
	return &serverv1.ListDatabasesResponse{Databases: []*serverv1.Database{{Id: "app"}}}, nil
}

func (s *fakeServices) CreateCollection(ctx context.Context, req *serverv1.CreateCollectionRequest) (*serverv1.Collection, error) {
	s.rec.mu.Lock()
	s.rec.lastCollection = req
	s.rec.mu.Unlock()
	return &serverv1.Collection{Id: req.Id, DatabaseId: req.DatabaseId, Name: req.Name, Permissions: req.Permissions}, nil
}

func (s *fakeServices) GetCollection(ctx context.Context, req *serverv1.GetCollectionRequest) (*serverv1.Collection, error) {
	return &serverv1.Collection{Id: req.CollectionId, DatabaseId: req.DatabaseId, Name: req.CollectionId}, nil
}

func (s *fakeServices) DeleteCollection(ctx context.Context, _ *serverv1.GetCollectionRequest) (*sharedv1.Empty, error) {
	if err := s.rec.fail("DeleteCollection"); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *fakeServices) UpdateCollection(ctx context.Context, req *serverv1.UpdateCollectionRequest) (*serverv1.Collection, error) {
	if err := s.rec.fail("UpdateCollection"); err != nil {
		return nil, err
	}
	s.rec.mu.Lock()
	s.rec.lastCollectionUpdate = req
	s.rec.mu.Unlock()
	return &serverv1.Collection{Id: req.CollectionId, DatabaseId: req.DatabaseId, Name: req.GetName()}, nil
}

func (s *fakeServices) DeleteAttribute(ctx context.Context, req *serverv1.DeleteAttributeRequest) (*sharedv1.Empty, error) {
	if err := s.rec.fail("DeleteAttribute"); err != nil {
		return nil, err
	}
	s.rec.mu.Lock()
	s.rec.deletedAttributeKey = req.Key
	s.rec.mu.Unlock()
	return &sharedv1.Empty{}, nil
}

func (s *fakeServices) DeleteIndex(ctx context.Context, req *serverv1.DeleteIndexRequest) (*sharedv1.Empty, error) {
	if err := s.rec.fail("DeleteIndex"); err != nil {
		return nil, err
	}
	s.rec.mu.Lock()
	s.rec.deletedIndexID = req.IndexId
	s.rec.mu.Unlock()
	return &sharedv1.Empty{}, nil
}

func (s *fakeServices) ListCollections(ctx context.Context, _ *serverv1.ListCollectionsRequest) (*serverv1.ListCollectionsResponse, error) {
	return &serverv1.ListCollectionsResponse{Collections: []*serverv1.Collection{{Id: "members"}}}, nil
}

func (s *fakeServices) CreateAttribute(ctx context.Context, req *serverv1.CreateAttributeRequest) (*serverv1.Attribute, error) {
	return &serverv1.Attribute{Id: "attr-" + req.Key, Key: req.Key, Type: req.Type, Required: req.Required, Array: req.Array}, nil
}

func (s *fakeServices) CreateIndex(ctx context.Context, req *serverv1.CreateIndexRequest) (*serverv1.Index, error) {
	return &serverv1.Index{Id: req.Id, Type: req.Type, Attributes: req.Attributes}, nil
}

func (s *fakeServices) CreateDocument(ctx context.Context, req *serverv1.CreateDocumentRequest) (*sharedv1.Document, error) {
	return &sharedv1.Document{Id: req.DocumentId, Data: req.Data, Permissions: req.Permissions}, nil
}

func (s *fakeServices) GetDocument(ctx context.Context, req *serverv1.GetDocumentRequest) (*sharedv1.Document, error) {
	return &sharedv1.Document{Id: req.DocumentId}, nil
}

func (s *fakeServices) UpdateDocument(ctx context.Context, req *serverv1.UpdateDocumentRequest) (*sharedv1.Document, error) {
	return &sharedv1.Document{Id: req.DocumentId, Data: req.Data, Permissions: req.Permissions}, nil
}

func (s *fakeServices) UpsertDocument(ctx context.Context, req *serverv1.UpsertDocumentRequest) (*sharedv1.Document, error) {
	s.rec.mu.Lock()
	s.rec.upserts = append(s.rec.upserts, req)
	s.rec.mu.Unlock()
	return &sharedv1.Document{Id: req.DocumentId, Data: req.Data, Permissions: req.Permissions}, nil
}

func (s *fakeServices) DeleteDocument(ctx context.Context, _ *serverv1.DeleteDocumentRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (s *fakeServices) ListDocuments(ctx context.Context, _ *serverv1.ListDocumentsRequest) (*serverv1.ListDocumentsResponse, error) {
	return &serverv1.ListDocumentsResponse{
		Documents: []*sharedv1.Document{{Id: "d1"}, {Id: "d2"}},
		Meta:      &sharedv1.ListResponseMeta{NextPageToken: "next-token"},
	}, nil
}

func (s *fakeServices) CountDocuments(ctx context.Context, _ *serverv1.CountDocumentsRequest) (*serverv1.CountDocumentsResponse, error) {
	return &serverv1.CountDocumentsResponse{Count: 42}, nil
}

func (s *fakeServices) BulkUpdateDocuments(ctx context.Context, req *serverv1.BulkUpdateDocumentsRequest) (*serverv1.BulkDocumentsResponse, error) {
	return &serverv1.BulkDocumentsResponse{Affected: int64(len(req.DocumentIds))}, nil
}

func (s *fakeServices) BulkDeleteDocuments(ctx context.Context, req *serverv1.BulkDeleteDocumentsRequest) (*serverv1.BulkDocumentsResponse, error) {
	return &serverv1.BulkDocumentsResponse{Affected: int64(len(req.DocumentIds))}, nil
}

// newServicesBufconn 启动注册了 Health/Users/Groups/Databases fake 的 bufconn gRPC 服务。
func newServicesBufconn(t *testing.T) (*bufconn.Listener, *recorder) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	rec := &recorder{}
	srv := grpc.NewServer()
	fake := &fakeServices{rec: rec}
	serverv1.RegisterHealthServiceServer(srv, fake)
	serverv1.RegisterUsersServiceServer(srv, fake)
	serverv1.RegisterGroupsServiceServer(srv, fake)
	serverv1.RegisterDatabasesServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis, rec
}

// newServicesClient 返回连接全量 fake 服务的 Server API 客户端。
func newServicesClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	lis, _ := newServicesBufconn(t)
	return newTestClient(t, lis, opts...)
}

func TestHealthCheckAndVersion(t *testing.T) {
	c := newServicesClient(t, WithAPIKey("key-1"))
	ctx := context.Background()

	check, err := c.Health.Check(ctx)
	require.NoError(t, err)
	require.Equal(t, "SERVING", check.Status)

	version, err := c.Health.GetVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, "v0.1.0", version.Version)
	require.Equal(t, "abc123", version.Commit)
}

func TestUsers_CreateUserAndToken(t *testing.T) {
	lis, rec := newServicesBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("key-1"))
	ctx := context.Background()

	user, err := c.Users.CreateUser(ctx, "agent-1@agents.local", "pw", "Agent One", "active", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "user-1", user.Id)
	require.Equal(t, "active", user.Status)

	rec.mu.Lock()
	require.Equal(t, "agent-1@agents.local", rec.createdUser.Email)
	require.Equal(t, "pw", rec.createdUser.Password)
	rec.mu.Unlock()

	got, err := c.Users.GetUser(ctx, "user-1")
	require.NoError(t, err)
	require.Equal(t, "agent-1@agents.local", got.Email)

	tok, err := c.Users.CreateUserToken(ctx, "user-1")
	require.NoError(t, err)
	require.Equal(t, "agent-token", tok.Tokens.AccessToken)
}

func TestUsers_ListSessionsAndDelete(t *testing.T) {
	c := newServicesClient(t, WithAPIKey("key-1"))
	ctx := context.Background()

	sessions, err := c.Users.ListUserSessions(ctx, "user-1")
	require.NoError(t, err)
	require.Len(t, sessions.Sessions, 1)

	require.NoError(t, c.Users.DeleteUserSession(ctx, "user-1", "sess-1"))
	require.NoError(t, c.Users.DeleteUser(ctx, "user-1"))
}

func TestGroups_CreateGroupAndMembership(t *testing.T) {
	c := newServicesClient(t, WithAPIKey("key-1"))
	ctx := context.Background()

	group, err := c.Groups.CreateGroup(ctx, "Group One", []string{"read"})
	require.NoError(t, err)
	require.Equal(t, "Group One", group.Name)
	require.Equal(t, []string{"read"}, group.Permissions)

	got, err := c.Groups.GetGroup(ctx, "group-1")
	require.NoError(t, err)
	require.Equal(t, "group-1", got.Id)

	mem, err := c.Groups.CreateMembership(ctx, "group-1", "user-1", "", "", []string{"member"}, "active")
	require.NoError(t, err)
	require.Equal(t, "user-1", mem.UserId)
	require.Equal(t, "active", mem.Status)

	listed, err := c.Groups.ListMemberships(ctx, "group-1")
	require.NoError(t, err)
	require.Len(t, listed.Memberships, 1)
}

func TestDatabases_SchemaSetup(t *testing.T) {
	lis, rec := newServicesBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("key-1"), WithDatabaseID("app"))
	ctx := context.Background()
	db := c.Databases

	created, err := db.CreateDatabase(ctx, "app", "Application DB")
	require.NoError(t, err)
	require.Equal(t, "app", created.Id)

	col, err := db.CreateCollection(ctx, "members", "Members",
		[]string{"read:user:*"}, true)
	require.NoError(t, err)
	require.Equal(t, "members", col.Id)
	require.Equal(t, "Members", col.Name)

	rec.mu.Lock()
	require.NotNil(t, rec.lastCollection)
	require.True(t, *rec.lastCollection.DocumentSecurity)
	require.Equal(t, []string{"read:user:*"}, rec.lastCollection.Permissions)
	rec.mu.Unlock()

	attr, err := db.CreateAttribute(ctx, "members", "channel_id", "string", 64, true, false)
	require.NoError(t, err)
	require.Equal(t, "channel_id", attr.Key)
	require.Equal(t, "string", attr.Type)
	require.True(t, attr.Required)

	idx, err := db.CreateIndex(ctx, "members", "members_channel_user", "unique",
		[]string{"channel_id", "user_id"})
	require.NoError(t, err)
	require.Equal(t, "unique", idx.Type)
	require.Equal(t, []string{"channel_id", "user_id"}, idx.Attributes)
}

func TestDatabases_CountAndListDocuments(t *testing.T) {
	c := newServicesClient(t, WithAPIKey("key-1"), WithDatabaseID("app"))
	ctx := context.Background()

	count, err := c.Databases.CountDocuments(ctx, "messages",
		&sharedv1.Query{Filter: eqFilter("channel_id", "ch1")})
	require.NoError(t, err)
	require.Equal(t, int64(42), count)

	res, err := c.Databases.ListDocuments(ctx, "messages",
		&sharedv1.Query{Filter: eqFilter("channel_id", "ch1"), PageSize: 20})
	require.NoError(t, err)
	require.Len(t, res.Documents, 2)
	require.Equal(t, "next-token", res.NextPageToken)
}

// eqFilter 是测试用的 equal 过滤构造捷径。
func eqFilter(attr string, values ...string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: &sharedv1.Comparison{
		Attribute: attr, Values: values,
	}}}
}

func TestDatabases_BulkOperations(t *testing.T) {
	c := newServicesClient(t, WithAPIKey("key-1"), WithDatabaseID("app"))
	ctx := context.Background()

	resp, err := c.Databases.BulkUpdateDocuments(ctx, "members", []string{"m1", "m2"},
		map[string]any{"last_read_seq": 1}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), resp.Affected)

	del, err := c.Databases.BulkDeleteDocuments(ctx, "members", []string{"m1"})
	require.NoError(t, err)
	require.Equal(t, int64(1), del.Affected)
}

func TestUsers_UpdateUserPassword(t *testing.T) {
	lis, rec := newServicesBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("key-1"))
	ctx := context.Background()

	user, err := c.Users.UpdateUserPassword(ctx, "user-1", "new-pw")
	require.NoError(t, err)
	require.Equal(t, "user-1", user.Id)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Equal(t, "user-1", rec.lastUserPassword.Id)
	require.Equal(t, "new-pw", rec.lastUserPassword.Password)
}

func TestGroups_DeleteAndPrefs(t *testing.T) {
	lis, rec := newServicesBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("key-1"))
	ctx := context.Background()

	require.NoError(t, c.Groups.DeleteGroup(ctx, "group-1"))

	prefs, err := c.Groups.GetGroupPrefs(ctx, "group-1")
	require.NoError(t, err)
	require.NotNil(t, prefs.Prefs)

	updated, err := c.Groups.UpdateGroupPrefs(ctx, "group-1", map[string]any{"locale": "zh-CN"})
	require.NoError(t, err)
	require.NotNil(t, updated.Prefs)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Equal(t, "group-1", rec.lastGroupPrefs.Id)
	require.Equal(t, "zh-CN", rec.lastGroupPrefs.Prefs.Fields["locale"].GetStringValue())
}

func TestDatabases_UpdateAndDeleteSchema(t *testing.T) {
	lis, rec := newServicesBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("key-1"), WithDatabaseID("app"))
	ctx := context.Background()
	db := c.Databases

	disabled := true
	security := false
	updated, err := db.UpdateCollection(ctx, "members", "Members v2",
		[]string{"read:all"}, &security, &disabled)
	require.NoError(t, err)
	require.Equal(t, "Members v2", updated.Name)

	rec.mu.Lock()
	require.Equal(t, "app", rec.lastCollectionUpdate.DatabaseId)
	require.Equal(t, "members", rec.lastCollectionUpdate.CollectionId)
	require.NotNil(t, rec.lastCollectionUpdate.Name)
	require.Equal(t, "Members v2", *rec.lastCollectionUpdate.Name)
	require.Equal(t, []string{"read:all"}, rec.lastCollectionUpdate.Permissions.Values)
	require.NotNil(t, rec.lastCollectionUpdate.DocumentSecurity)
	require.False(t, *rec.lastCollectionUpdate.DocumentSecurity)
	require.NotNil(t, rec.lastCollectionUpdate.Disabled)
	require.True(t, *rec.lastCollectionUpdate.Disabled)
	rec.mu.Unlock()

	require.NoError(t, db.DeleteCollection(ctx, "members"))

	require.NoError(t, db.DeleteAttribute(ctx, "members", "channel_id"))
	rec.mu.Lock()
	require.Equal(t, "channel_id", rec.deletedAttributeKey)
	rec.mu.Unlock()

	require.NoError(t, db.DeleteIndex(ctx, "members", "members_channel_user"))
	rec.mu.Lock()
	require.Equal(t, "members_channel_user", rec.deletedIndexID)
	rec.mu.Unlock()
}

// TestF84Methods_ErrorPropagation 覆盖 F8-4 新增 8 个类型化方法的错误路径：
// 服务端返回 NotFound/PermissionDenied 时 SDK 必须原样透传（不吞错、不改码）。
func TestF84Methods_ErrorPropagation(t *testing.T) {
	lis, rec := newServicesBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("key-1"), WithDatabaseID("app"))
	ctx := context.Background()

	notFound := status.Error(codes.NotFound, "resource not found")
	denied := status.Error(codes.PermissionDenied, "permission denied")

	cases := []struct {
		name string
		rpc  string
		err  error
		call func() error
	}{
		{name: "UpdateUserPassword NotFound", rpc: "UpdateUserPassword", err: notFound,
			call: func() error { _, err := c.Users.UpdateUserPassword(ctx, "user-1", "pw"); return err }},
		{name: "DeleteGroup NotFound", rpc: "DeleteGroup", err: notFound,
			call: func() error { return c.Groups.DeleteGroup(ctx, "group-1") }},
		{name: "GetGroupPrefs PermissionDenied", rpc: "GetGroupPrefs", err: denied,
			call: func() error { _, err := c.Groups.GetGroupPrefs(ctx, "group-1"); return err }},
		{name: "UpdateGroupPrefs NotFound", rpc: "UpdateGroupPrefs", err: notFound,
			call: func() error {
				_, err := c.Groups.UpdateGroupPrefs(ctx, "group-1", map[string]any{"locale": "zh"})
				return err
			}},
		{name: "UpdateCollection NotFound", rpc: "UpdateCollection", err: notFound,
			call: func() error { _, err := c.Databases.UpdateCollection(ctx, "members", "M", nil, nil, nil); return err }},
		{name: "DeleteCollection PermissionDenied", rpc: "DeleteCollection", err: denied,
			call: func() error { return c.Databases.DeleteCollection(ctx, "members") }},
		{name: "DeleteAttribute NotFound", rpc: "DeleteAttribute", err: notFound,
			call: func() error { return c.Databases.DeleteAttribute(ctx, "members", "channel_id") }},
		{name: "DeleteIndex NotFound", rpc: "DeleteIndex", err: notFound,
			call: func() error { return c.Databases.DeleteIndex(ctx, "members", "idx-1") }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec.setErr(tc.rpc, tc.err)
			defer rec.setErr(tc.rpc, nil)
			err := tc.call()
			require.Error(t, err)
			require.Equal(t, tc.err.Error(), err.Error())
			require.Equal(t, status.Code(tc.err), status.Code(err))
		})
	}
}
