package clientgrpc

import (
	"context"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/app/client"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type GroupsService struct {
	clientv1.UnimplementedGroupsServiceServer
	groups *client.Groups
}

func NewGroupsService(groups *client.Groups) *GroupsService {
	return &GroupsService{groups: groups}
}

func (s *GroupsService) CreateGroup(ctx context.Context, req *clientv1.CreateGroupRequest) (*clientv1.Group, error) {
	doc, err := s.groups.CreateGroup(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	return mapClientGroupDoc(doc), nil
}

func (s *GroupsService) ListGroups(ctx context.Context, req *sharedv1.ListRequest) (*clientv1.ListGroupsResponse, error) {
	docs, total, next, err := s.groups.ListGroups(ctx, databases.Query{
		Queries:   req.GetQueries(),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.Group, len(docs))
	for i := range docs {
		out[i] = mapClientGroupDoc(&docs[i])
	}
	return &clientv1.ListGroupsResponse{
		Groups: out,
		Meta:   &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), TotalCount: int32(total), NextPageToken: next},
	}, nil
}

func (s *GroupsService) GetGroup(ctx context.Context, req *clientv1.GetGroupRequest) (*clientv1.Group, error) {
	doc, err := s.groups.GetGroup(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, status.Error(codes.NotFound, "group not found")
	}
	return mapClientGroupDoc(doc), nil
}

func (s *GroupsService) DeleteGroup(ctx context.Context, req *clientv1.GetGroupRequest) (*sharedv1.Empty, error) {
	if err := s.groups.DeleteGroup(ctx, req.GetId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *GroupsService) CreateMembership(ctx context.Context, req *clientv1.CreateMembershipRequest) (*clientv1.Membership, error) {
	doc, err := s.groups.CreateMembership(ctx, req.GetGroupId(), req.GetEmail(), req.GetName(), req.GetRoles())
	if err != nil {
		return nil, err
	}
	return mapClientMembershipDoc(doc), nil
}

func (s *GroupsService) ListMemberships(ctx context.Context, req *clientv1.ListMembershipsRequest) (*clientv1.ListMembershipsResponse, error) {
	docs, err := s.groups.ListMemberships(ctx, req.GetGroupId())
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.Membership, len(docs))
	for i := range docs {
		out[i] = mapClientMembershipDoc(&docs[i])
	}
	return &clientv1.ListMembershipsResponse{Memberships: out}, nil
}

func (s *GroupsService) UpdateMembershipStatus(ctx context.Context, req *clientv1.UpdateMembershipStatusRequest) (*clientv1.Membership, error) {
	doc, err := s.groups.UpdateMembershipStatus(ctx, req.GetGroupId(), req.GetMembershipId(), req.GetStatus())
	if err != nil {
		return nil, err
	}
	return mapClientMembershipDoc(doc), nil
}

func (s *GroupsService) DeleteMembership(ctx context.Context, req *clientv1.GetMembershipRequest) (*sharedv1.Empty, error) {
	if err := s.groups.DeleteMembership(ctx, req.GetGroupId(), req.GetMembershipId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func mapClientGroupDoc(doc *databases.Document) *clientv1.Group {
	if doc == nil {
		return nil
	}
	t := &clientv1.Group{
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
	return t
}

func mapClientMembershipDoc(doc *databases.Document) *clientv1.Membership {
	if doc == nil {
		return nil
	}
	m := &clientv1.Membership{
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
	m.InvitedAt = clientDocTimeField(doc.Data, "invited_at")
	m.JoinedAt = clientDocTimeField(doc.Data, "joined_at")
	return m
}

func clientDocTimeField(data map[string]any, key string) *timestamppb.Timestamp {
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
