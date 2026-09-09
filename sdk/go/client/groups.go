package client

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// GroupsService 封装 Client API 的 Groups 服务。
type GroupsService struct{ c *Client }

// CreateGroup 创建用户组。
func (t *GroupsService) CreateGroup(ctx context.Context, name string) (*clientv1.Group, error) {
	return t.c.groups.CreateGroup(ctx, &clientv1.CreateGroupRequest{Name: name})
}

// GetGroup 按 ID 获取用户组。
func (t *GroupsService) GetGroup(ctx context.Context, groupID string) (*clientv1.Group, error) {
	return t.c.groups.GetGroup(ctx, &clientv1.GetGroupRequest{Id: groupID})
}

// ListGroups 列出当前用户所属用户组。
func (t *GroupsService) ListGroups(ctx context.Context) (*clientv1.ListGroupsResponse, error) {
	return t.c.groups.ListGroups(ctx, &sharedv1.ListRequest{})
}

// CreateMembership 邀请成员加入用户组（按邮箱）。
func (t *GroupsService) CreateMembership(ctx context.Context, groupID, email, name string, roles []string) (*clientv1.Membership, error) {
	return t.c.groups.CreateMembership(ctx, &clientv1.CreateMembershipRequest{
		GroupId: groupID,
		Email:   email,
		Name:    name,
		Roles:   roles,
	})
}

// ListMemberships 列出用户组成员。
func (t *GroupsService) ListMemberships(ctx context.Context, groupID string) (*clientv1.ListMembershipsResponse, error) {
	return t.c.groups.ListMemberships(ctx, &clientv1.ListMembershipsRequest{GroupId: groupID})
}

// UpdateMembershipStatus 更新成员状态（如 active / invited / banned）。
func (t *GroupsService) UpdateMembershipStatus(ctx context.Context, groupID, membershipID, status string) (*clientv1.Membership, error) {
	return t.c.groups.UpdateMembershipStatus(ctx, &clientv1.UpdateMembershipStatusRequest{
		GroupId:      groupID,
		MembershipId: membershipID,
		Status:       status,
	})
}

// DeleteGroup 删除用户组（仅 owner 角色；Round3 H4-3 补齐与 TS/proto 对齐）。
func (t *GroupsService) DeleteGroup(ctx context.Context, groupID string) error {
	_, err := t.c.groups.DeleteGroup(ctx, &clientv1.GetGroupRequest{Id: groupID})
	return err
}

// DeleteMembership 移除用户组成员。
func (t *GroupsService) DeleteMembership(ctx context.Context, groupID, membershipID string) error {
	_, err := t.c.groups.DeleteMembership(ctx, &clientv1.GetMembershipRequest{
		GroupId:      groupID,
		MembershipId: membershipID,
	})
	return err
}
