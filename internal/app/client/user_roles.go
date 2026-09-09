package client

import (
	"context"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
)

// UserRoles resolves JWT role claims for a user from static system tables.
type UserRoles struct {
	users       users.Repository
	memberships groups.MembershipRepository
}

func NewUserRoles(usersRepo users.Repository, memberships groups.MembershipRepository) *UserRoles {
	return &UserRoles{users: usersRepo, memberships: memberships}
}

// LoadUserRoles 解析端用户 JWT 角色声明（签发时 + validator 每请求）。
// u 是调用方已取回的 users 行：非空直接使用（删掉本函数历史上对同一行的
// 第二次 users.GetByID——P0.5 热路径清账）；nil 时兜底单查，行为不变。
func (r *UserRoles) LoadUserRoles(ctx context.Context, projectID, userID string, u *users.User) ([]string, error) {
	// 角色形态一律取自 DocRole 词表（M6.1 主权），禁止裸串拼接。
	baseRoles := []string{databases.RoleUsers, databases.RoleUser(userID)}
	if r.users == nil {
		return baseRoles, nil
	}
	found := u
	if found == nil {
		var err error
		found, err = r.users.GetByID(ctx, projectID, userID)
		if err != nil {
			return baseRoles, err
		}
	}
	if found == nil {
		return baseRoles, nil
	}
	if found.EmailVerified {
		baseRoles = append(baseRoles, databases.RoleUserVerified(userID))
	}
	for _, label := range found.Labels {
		if label != "" {
			baseRoles = append(baseRoles, databases.RoleLabel(label))
		}
	}
	groupRoles, err := r.loadGroupRoles(ctx, projectID, userID)
	if err != nil {
		return baseRoles, err
	}
	return append(baseRoles, groupRoles...), nil
}

func (r *UserRoles) loadGroupRoles(ctx context.Context, projectID, userID string) ([]string, error) {
	if userID == "" || r.memberships == nil {
		return nil, nil
	}
	list, err := r.memberships.ListByUser(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list)*3)
	for _, m := range list {
		if m.Status != groups.StatusAccepted || m.GroupID == "" {
			continue
		}
		out = append(out, databases.RoleGroup(m.GroupID), databases.RoleMembership(m.ID))
		for _, role := range m.Roles {
			if role != "" {
				out = append(out, databases.RoleGroupRole(m.GroupID, role))
			}
		}
	}
	return out, nil
}
