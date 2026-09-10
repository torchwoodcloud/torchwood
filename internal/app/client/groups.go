package client

import (
	"context"
	"slices"

	"github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Groups struct {
	groups    *server.Groups
	usersRepo users.Repository
}

func NewGroups(groups *server.Groups, usersRepo users.Repository) *Groups {
	return &Groups{groups: groups, usersRepo: usersRepo}
}

func (t *Groups) dbPrincipal(ctx context.Context) (projectID, userID, email string, principal databases.Principal, err error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p.ProjectID == "" || p.UserID == "" {
		return "", "", "", databases.Principal{}, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	return p.ProjectID, p.UserID, p.Email, p.DocPrincipal(), nil
}

func (t *Groups) CreateGroup(ctx context.Context, name string) (*databases.Document, error) {
	projectID, userID, email, principal, err := t.dbPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	group, _, err := t.groups.CreateGroupWithOwner(ctx, projectID, name, userID, email, principal)
	return group, err
}

func (t *Groups) ListGroups(ctx context.Context, q databases.Query) ([]databases.Document, int64, string, error) {
	projectID, _, _, principal, err := t.dbPrincipal(ctx)
	if err != nil {
		return nil, 0, "", err
	}
	return t.groups.ListGroups(ctx, projectID, q, principal)
}

func (t *Groups) GetGroup(ctx context.Context, groupID string) (*databases.Document, error) {
	projectID, _, _, principal, err := t.dbPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	return t.groups.GetGroup(ctx, projectID, groupID, principal)
}

func (t *Groups) DeleteGroup(ctx context.Context, groupID string) error {
	projectID, userID, _, principal, err := t.dbPrincipal(ctx)
	if err != nil {
		return err
	}
	labels, err := t.groups.AcceptedGroupRoleLabels(ctx, projectID, userID)
	if err != nil {
		return err
	}
	if !slices.Contains(labels[groupID], groups.RoleOwner) {
		return status.Error(codes.PermissionDenied, "only the group owner can delete the group")
	}
	return t.groups.DeleteGroup(ctx, projectID, groupID, principal)
}

func (t *Groups) CreateMembership(ctx context.Context, groupID, inviteEmail, name string, roles []string) (*databases.Document, error) {
	projectID, userID, _, principal, err := t.dbPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := t.groups.AcceptedGroupRoleLabels(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if len(labels[groupID]) == 0 {
		return nil, status.Error(codes.PermissionDenied, "not a member of this group")
	}
	for _, role := range roles {
		if role != groups.RoleMember {
			return nil, status.Error(codes.PermissionDenied, "only member role can be assigned when creating membership")
		}
	}
	return t.groups.CreateMembership(ctx, projectID, server.CreateMembershipCommand{
		GroupID: groupID,
		Email:   inviteEmail,
		Name:    name,
		Roles:   roles,
		Status:  groups.StatusPending,
	}, principal)
}

func (t *Groups) ListMemberships(ctx context.Context, groupID string) ([]databases.Document, error) {
	projectID, _, _, principal, err := t.dbPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	docs, _, _, err := t.groups.ListMemberships(ctx, projectID, groupID, databases.Query{}, principal)
	return docs, err
}

func (t *Groups) UpdateMembershipStatus(ctx context.Context, groupID, membershipID, statusVal string) (*databases.Document, error) {
	projectID, userID, email, principal, err := t.dbPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	doc, err := t.groups.GetMembership(ctx, projectID, groupID, membershipID, principal)
	if err != nil {
		return nil, err
	}
	memUserID, _ := doc.Data["user_id"].(string)
	memEmail, _ := doc.Data["email"].(string)
	if memUserID != userID && memEmail != email {
		return nil, status.Error(codes.PermissionDenied, "cannot update another user's membership")
	}
	if statusVal == groups.StatusAccepted {
		// 接受邀请一律要求调用者邮箱已验证：SignUp 不强制验证邮箱，
		// 若邀请创建时目标邮箱已被未验证账号抢注（user_id 绑定为该账号），
		// 仅按 memUserID 判断会绕过验证，因此这里无条件校验 email_verified。
		found, err := t.usersRepo.GetByID(ctx, projectID, userID)
		if err != nil {
			return nil, err
		}
		verified := found != nil && found.EmailVerified
		if !verified {
			return nil, status.Error(codes.FailedPrecondition, "email verification required to accept group invitation")
		}
	}
	return t.groups.UpdateMembershipStatus(ctx, projectID, groupID, membershipID, statusVal, principal)
}

func (t *Groups) DeleteMembership(ctx context.Context, groupID, membershipID string) error {
	projectID, userID, email, principal, err := t.dbPrincipal(ctx)
	if err != nil {
		return err
	}
	doc, err := t.groups.GetMembership(ctx, projectID, groupID, membershipID, principal)
	if err != nil {
		return err
	}
	memUserID, _ := doc.Data["user_id"].(string)
	memEmail, _ := doc.Data["email"].(string)
	if memUserID != userID && memEmail != email {
		labels, err := t.groups.AcceptedGroupRoleLabels(ctx, projectID, userID)
		if err != nil {
			return err
		}
		if !slices.Contains(labels[groupID], groups.RoleOwner) {
			return status.Error(codes.PermissionDenied, "only the group owner can remove other members")
		}
	}
	return t.groups.DeleteMembership(ctx, projectID, groupID, membershipID, principal)
}
