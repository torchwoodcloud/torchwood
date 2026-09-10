package servergrpc

import (
	"context"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type GroupsService struct {
	serverv1.UnimplementedGroupsServiceServer
	groups *appserver.Groups
}

func NewGroupsService(groups *appserver.Groups) *GroupsService {
	return &GroupsService{groups: groups}
}

func (s *GroupsService) projectID(ctx context.Context) string {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return ""
	}
	return p.ProjectID
}

func (s *GroupsService) CreateGroup(ctx context.Context, req *serverv1.CreateGroupRequest) (*serverv1.Group, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.groups.CreateGroup(ctx, projectID, req.GetName(), req.GetPermissions())
	if err != nil {
		return nil, err
	}
	return mapGroupDoc(doc), nil
}

func (s *GroupsService) ListGroups(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListGroupsResponse, error) {
	// groups 面不消费查询过滤：显式拒绝（R12b，同 storage 法），不再静默忽略。
	if err := rejectListQueries(req.GetQueries()); err != nil {
		return nil, err
	}
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	docs, total, next, err := s.groups.ListGroups(ctx, projectID, databases.Query{
		Queries:   req.GetQueries(),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	}, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.Group, len(docs))
	for i := range docs {
		out[i] = mapGroupDoc(&docs[i])
	}
	return &serverv1.ListGroupsResponse{
		Groups: out,
		Meta:   &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), TotalCount: int32(total), NextPageToken: next},
	}, nil
}

func (s *GroupsService) GetGroup(ctx context.Context, req *serverv1.GetGroupRequest) (*serverv1.Group, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.groups.GetGroup(ctx, projectID, req.GetId(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, status.Error(codes.NotFound, "group not found")
	}
	return mapGroupDoc(doc), nil
}

func (s *GroupsService) DeleteGroup(ctx context.Context, req *serverv1.GetGroupRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if err := s.groups.DeleteGroup(ctx, projectID, req.GetId(), dbPrincipal(ctx)); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *GroupsService) GetGroupPrefs(ctx context.Context, req *serverv1.GetGroupRequest) (*serverv1.GetGroupPrefsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	prefs, err := s.groups.GetGroupPrefs(ctx, projectID, req.GetId(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	data, err := structpb.NewStruct(prefs)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "prefs is not serializable")
	}
	return &serverv1.GetGroupPrefsResponse{Prefs: data}, nil
}

func (s *GroupsService) UpdateGroupPrefs(ctx context.Context, req *serverv1.UpdateGroupPrefsRequest) (*serverv1.GetGroupPrefsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	// prefs required 由 UpdateGroupPrefsRequest 的 buf.validate 注解在
	// validate 拦截器承担（09-api-guide §2.3）；app 层防线保留。
	prefs, err := s.groups.UpdateGroupPrefs(ctx, projectID, req.GetId(), req.GetPrefs().AsMap(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	data, err := structpb.NewStruct(prefs)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "prefs is not serializable")
	}
	return &serverv1.GetGroupPrefsResponse{Prefs: data}, nil
}

func (s *GroupsService) CreateMembership(ctx context.Context, req *serverv1.CreateMembershipRequest) (*serverv1.Membership, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.groups.CreateMembership(ctx, projectID, appserver.CreateMembershipCommand{
		GroupID: req.GetGroupId(),
		UserID:  req.GetUserId(),
		Email:   req.GetEmail(),
		Name:    req.GetName(),
		Roles:   req.GetRoles(),
		Status:  req.GetStatus(),
	}, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return mapMembershipDoc(doc), nil
}

func (s *GroupsService) ListMemberships(ctx context.Context, req *serverv1.ListMembershipsRequest) (*serverv1.ListMembershipsResponse, error) {
	// groups 面不消费查询过滤：显式拒绝（R12b，同 storage 法）；成员定位走
	// group_id 路径参数。
	if err := rejectListQueries(req.GetQueries()); err != nil {
		return nil, err
	}
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	docs, total, next, err := s.groups.ListMemberships(ctx, projectID, req.GetGroupId(), databases.Query{
		Queries:   req.GetQueries(),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	}, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.Membership, len(docs))
	for i := range docs {
		out[i] = mapMembershipDoc(&docs[i])
	}
	return &serverv1.ListMembershipsResponse{
		Memberships: out,
		Meta:        &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), TotalCount: int32(total), NextPageToken: next},
	}, nil
}

func (s *GroupsService) GetMembership(ctx context.Context, req *serverv1.GetMembershipRequest) (*serverv1.Membership, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.groups.GetMembership(ctx, projectID, req.GetGroupId(), req.GetMembershipId(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, status.Error(codes.NotFound, "membership not found")
	}
	return mapMembershipDoc(doc), nil
}

func (s *GroupsService) UpdateMembership(ctx context.Context, req *serverv1.UpdateMembershipRequest) (*serverv1.Membership, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.groups.UpdateMembership(ctx, projectID, req.GetGroupId(), req.GetMembershipId(), appserver.UpdateMembershipCommand{
		Roles: req.GetRoles(),
	}, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return mapMembershipDoc(doc), nil
}

func (s *GroupsService) UpdateMembershipStatus(ctx context.Context, req *serverv1.UpdateMembershipStatusRequest) (*serverv1.Membership, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.groups.UpdateMembershipStatus(ctx, projectID, req.GetGroupId(), req.GetMembershipId(), req.GetStatus(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return mapMembershipDoc(doc), nil
}

func (s *GroupsService) DeleteMembership(ctx context.Context, req *serverv1.GetMembershipRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if err := s.groups.DeleteMembership(ctx, projectID, req.GetGroupId(), req.GetMembershipId(), dbPrincipal(ctx)); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func mapGroupDoc(doc *databases.Document) *serverv1.Group {
	if doc == nil {
		return nil
	}
	t := &serverv1.Group{
		Id:        doc.ID,
		CreatedAt: timestamppb.New(doc.CreatedAt),
		UpdatedAt: timestamppb.New(doc.UpdatedAt),
	}
	if v, ok := doc.Data["name"].(string); ok {
		t.Name = v
	}
	if v, ok := doc.Data["total"].(float64); ok {
		t.Total = int32(v)
	}
	if v, ok := doc.Data["total"].(int64); ok {
		t.Total = int32(v)
	}
	if arr, ok := doc.Data["permissions"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				t.Permissions = append(t.Permissions, s)
			}
		}
	}
	return t
}

func mapMembershipDoc(doc *databases.Document) *serverv1.Membership {
	if doc == nil {
		return nil
	}
	m := &serverv1.Membership{
		Id:        doc.ID,
		CreatedAt: timestamppb.New(doc.CreatedAt),
		UpdatedAt: timestamppb.New(doc.UpdatedAt),
	}
	if v, ok := doc.Data["group_id"].(string); ok {
		m.GroupId = v
	}
	if v, ok := doc.Data["user_id"].(string); ok {
		m.UserId = v
	}
	if v, ok := doc.Data["email"].(string); ok {
		m.Email = v
	}
	if v, ok := doc.Data["name"].(string); ok {
		m.Name = v
	}
	if v, ok := doc.Data["status"].(string); ok {
		m.Status = v
	}
	if arr, ok := doc.Data["roles"].([]any); ok {
		for _, item := range arr {
			if s, ok := item.(string); ok {
				m.Roles = append(m.Roles, s)
			}
		}
	}
	m.InvitedAt = docTimeField(doc.Data, "invited_at")
	m.JoinedAt = docTimeField(doc.Data, "joined_at")
	return m
}

func docTimeField(data map[string]any, key string) *timestamppb.Timestamp {
	v, ok := data[key].(string)
	if !ok || v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}
