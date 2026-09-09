package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// GroupsService 封装 Server API 的用户组管理服务。
type GroupsService struct{ c *Client }

// CreateGroup 创建用户组（可附带权限）。
func (t *GroupsService) CreateGroup(ctx context.Context, name string, permissions []string) (*serverv1.Group, error) {
	return t.c.groups.CreateGroup(ctx, &serverv1.CreateGroupRequest{
		Name:        name,
		Permissions: permissions,
	})
}

// GetGroup 按 ID 获取用户组。
func (t *GroupsService) GetGroup(ctx context.Context, groupID string) (*serverv1.Group, error) {
	return t.c.groups.GetGroup(ctx, &serverv1.GetGroupRequest{Id: groupID})
}

// DeleteGroup 删除用户组。
func (t *GroupsService) DeleteGroup(ctx context.Context, groupID string) error {
	_, err := t.c.groups.DeleteGroup(ctx, &serverv1.GetGroupRequest{Id: groupID})
	return err
}

// GetGroupPrefs 获取用户组偏好。
func (t *GroupsService) GetGroupPrefs(ctx context.Context, groupID string) (*serverv1.GetGroupPrefsResponse, error) {
	return t.c.groups.GetGroupPrefs(ctx, &serverv1.GetGroupRequest{Id: groupID})
}

// UpdateGroupPrefs 全量替换用户组偏好。
func (t *GroupsService) UpdateGroupPrefs(ctx context.Context, groupID string, prefs map[string]any) (*serverv1.GetGroupPrefsResponse, error) {
	prefsStruct, err := toStruct(prefs)
	if err != nil {
		return nil, err
	}
	return t.c.groups.UpdateGroupPrefs(ctx, &serverv1.UpdateGroupPrefsRequest{
		Id:    groupID,
		Prefs: prefsStruct,
	})
}

// ListGroups 按查询 DSL 列出用户组。
func (t *GroupsService) ListGroups(ctx context.Context, queries []string, pageSize int32, pageToken string) (*serverv1.ListGroupsResponse, error) {
	return t.c.groups.ListGroups(ctx, &sharedv1.ListRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
		Queries:   queries,
	})
}

// CreateMembership 创建用户组成员关系（按 userID 或邮箱）。
func (t *GroupsService) CreateMembership(ctx context.Context, groupID, userID, email, name string, roles []string, status string) (*serverv1.Membership, error) {
	return t.c.groups.CreateMembership(ctx, &serverv1.CreateMembershipRequest{
		GroupId: groupID,
		UserId:  userID,
		Email:   email,
		Name:    name,
		Roles:   roles,
		Status:  status,
	})
}

// ListMemberships 列出用户组成员。
func (t *GroupsService) ListMemberships(ctx context.Context, groupID string) (*serverv1.ListMembershipsResponse, error) {
	return t.c.groups.ListMemberships(ctx, &serverv1.ListMembershipsRequest{GroupId: groupID})
}

// GetMembership 获取成员关系。
func (t *GroupsService) GetMembership(ctx context.Context, groupID, membershipID string) (*serverv1.Membership, error) {
	return t.c.groups.GetMembership(ctx, &serverv1.GetMembershipRequest{
		GroupId:      groupID,
		MembershipId: membershipID,
	})
}

// UpdateMembership 更新成员角色。
func (t *GroupsService) UpdateMembership(ctx context.Context, groupID, membershipID string, roles []string) (*serverv1.Membership, error) {
	return t.c.groups.UpdateMembership(ctx, &serverv1.UpdateMembershipRequest{
		GroupId:      groupID,
		MembershipId: membershipID,
		Roles:        roles,
	})
}

// UpdateMembershipStatus 更新成员状态。
func (t *GroupsService) UpdateMembershipStatus(ctx context.Context, groupID, membershipID, status string) (*serverv1.Membership, error) {
	return t.c.groups.UpdateMembershipStatus(ctx, &serverv1.UpdateMembershipStatusRequest{
		GroupId:      groupID,
		MembershipId: membershipID,
		Status:       status,
	})
}

// DeleteMembership 移除成员关系。
func (t *GroupsService) DeleteMembership(ctx context.Context, groupID, membershipID string) error {
	_, err := t.c.groups.DeleteMembership(ctx, &serverv1.GetMembershipRequest{
		GroupId:      groupID,
		MembershipId: membershipID,
	})
	return err
}
