package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/groups"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/pkg/crud"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Groups struct {
	projectRepo projects.Repository
	usersRepo   users.Repository
	groupsRepo  groups.GroupRepository
	memberships groups.MembershipRepository
}

func NewGroups(
	projectRepo projects.Repository,
	usersRepo users.Repository,
	groupsRepo groups.GroupRepository,
	memberships groups.MembershipRepository,
) *Groups {
	return &Groups{
		projectRepo: projectRepo,
		usersRepo:   usersRepo,
		groupsRepo:  groupsRepo,
		memberships: memberships,
	}
}

type CreateMembershipCommand struct {
	GroupID string
	UserID  string
	Email   string
	Name    string
	Roles   []string
	Status  string
}

type UpdateMembershipCommand struct {
	Roles []string
}

func (t *Groups) resolveProject(ctx context.Context, projectID string) (*projects.Project, error) {
	p, err := t.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return p, nil
}

func (t *Groups) getGroup(ctx context.Context, projectID, groupID string) (*groups.Group, error) {
	g, err := t.groupsRepo.GetByID(ctx, projectID, groupID)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, status.Error(codes.NotFound, "group not found")
	}
	return g, nil
}

// groupsWriteGuard 是 Groups 写方法的双面入口守卫（纵深防御，fail-closed）：
// server 面（console admin 会话 / API key）或端用户任一放行；匿名与 System
// 一律拒绝。Groups 由 client 面（端用户带项目绑定）与 server 面共享。
func groupsWriteGuard(ctx context.Context) error {
	return appshared.RequireAnyOf(ctx, appshared.RequireServerPrincipal, appshared.RequireEndUser)
}

func (t *Groups) CreateGroup(ctx context.Context, projectID, name string, perms []string) (*databases.Document, error) {
	if err := groupsWriteGuard(ctx); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	groupID := idgen.UUID().String()
	now := time.Now()
	g := &groups.Group{
		ID:          groupID,
		Name:        name,
		Permissions: perms,
		Total:       0,
		Prefs:       map[string]any{},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := t.groupsRepo.Insert(ctx, projectID, g); err != nil {
		return nil, fmt.Errorf("create group: %w", err)
	}
	return groupAsDocument(g), nil
}

func (t *Groups) CreateGroupWithOwner(ctx context.Context, projectID, name, ownerUserID, ownerEmail string, principal databases.Principal) (*databases.Document, *databases.Document, error) {
	if err := groupsWriteGuard(ctx); err != nil {
		return nil, nil, err
	}
	group, err := t.CreateGroup(ctx, projectID, name, nil)
	if err != nil {
		return nil, nil, err
	}
	membership, err := t.CreateMembership(ctx, projectID, CreateMembershipCommand{
		GroupID: group.ID,
		UserID:  ownerUserID,
		Email:   ownerEmail,
		Roles:   []string{groups.RoleOwner},
		Status:  groups.StatusAccepted,
	}, principal)
	if err != nil {
		_ = t.groupsRepo.Delete(ctx, projectID, group.ID)
		return nil, nil, err
	}
	group, err = t.GetGroup(ctx, projectID, group.ID, databases.SystemPrincipal)
	if err != nil {
		return nil, nil, err
	}
	return group, membership, nil
}

func (t *Groups) ListGroups(ctx context.Context, projectID string, q databases.Query, _ databases.Principal) ([]databases.Document, int64, string, error) {
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, 0, "", err
	}
	list, err := t.groupsRepo.List(ctx, projectID)
	if err != nil {
		return nil, 0, "", err
	}
	docs := make([]databases.Document, 0, len(list))
	for _, g := range list {
		if d := groupAsDocument(g); d != nil {
			docs = append(docs, *d)
		}
	}
	return paginateDocuments(docs, q.PageSize, q.PageToken)
}

func (t *Groups) GetGroup(ctx context.Context, projectID, groupID string, _ databases.Principal) (*databases.Document, error) {
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	g, err := t.getGroup(ctx, projectID, groupID)
	if err != nil {
		return nil, err
	}
	return groupAsDocument(g), nil
}

func (t *Groups) GetGroupPrefs(ctx context.Context, projectID, groupID string, _ databases.Principal) (map[string]any, error) {
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	g, err := t.getGroup(ctx, projectID, groupID)
	if err != nil {
		return nil, err
	}
	if g.Prefs == nil {
		return map[string]any{}, nil
	}
	return g.Prefs, nil
}

func (t *Groups) UpdateGroupPrefs(ctx context.Context, projectID, groupID string, prefs map[string]any, _ databases.Principal) (map[string]any, error) {
	if err := groupsWriteGuard(ctx); err != nil {
		return nil, err
	}
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	if prefs == nil {
		return nil, status.Error(codes.InvalidArgument, "prefs is required")
	}
	if _, err := t.getGroup(ctx, projectID, groupID); err != nil {
		return nil, err
	}
	if err := t.groupsRepo.Update(ctx, projectID, groupID, map[string]any{"prefs": prefs}); err != nil {
		return nil, appshared.MapDocumentDBError(err)
	}
	g, err := t.getGroup(ctx, projectID, groupID)
	if err != nil {
		return nil, err
	}
	if g.Prefs == nil {
		return map[string]any{}, nil
	}
	return g.Prefs, nil
}

func (t *Groups) DeleteGroup(ctx context.Context, projectID, groupID string, _ databases.Principal) error {
	if err := groupsWriteGuard(ctx); err != nil {
		return err
	}
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if _, err := t.getGroup(ctx, projectID, groupID); err != nil {
		return err
	}
	return t.groupsRepo.Delete(ctx, projectID, groupID)
}

func (t *Groups) CreateMembership(ctx context.Context, projectID string, cmd CreateMembershipCommand, _ databases.Principal) (*databases.Document, error) {
	if err := groupsWriteGuard(ctx); err != nil {
		return nil, err
	}
	if cmd.GroupID == "" {
		return nil, status.Error(codes.InvalidArgument, "group_id is required")
	}
	if cmd.UserID == "" && cmd.Email == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id or email is required")
	}
	cmd.Email = normalizeEmail(cmd.Email)
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	if _, err := t.getGroup(ctx, projectID, cmd.GroupID); err != nil {
		return nil, err
	}

	statusVal := cmd.Status
	if statusVal == "" {
		statusVal = groups.StatusPending
	}
	if err := groups.ValidateStatus(statusVal); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	membershipRoles := cmd.Roles
	if len(membershipRoles) == 0 {
		membershipRoles = []string{groups.RoleMember}
	}
	for _, role := range membershipRoles {
		if err := groups.ValidateRole(role); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	userID := cmd.UserID
	if userID == "" && statusVal == groups.StatusAccepted {
		return nil, status.Error(codes.InvalidArgument, "user_id is required for accepted membership")
	}
	if userID == "" && cmd.Email != "" {
		resolved, err := t.resolveUserIDByEmail(ctx, projectID, cmd.Email)
		if err != nil {
			return nil, err
		}
		userID = resolved
	}

	now := time.Now()
	m := &groups.Membership{
		ID:        idgen.UUID().String(),
		GroupID:   cmd.GroupID,
		UserID:    userID,
		Email:     cmd.Email,
		Name:      cmd.Name,
		Roles:     membershipRoles,
		Status:    statusVal,
		InvitedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if statusVal == groups.StatusAccepted {
		m.JoinedAt = now
	}
	if err := t.memberships.Insert(ctx, projectID, m); err != nil {
		if errors.Is(err, groups.ErrMembershipAlreadyExists) {
			return nil, status.Error(codes.AlreadyExists, "membership already exists")
		}
		return nil, fmt.Errorf("create membership: %w", err)
	}
	got, err := t.memberships.GetByID(ctx, projectID, m.ID)
	if err != nil {
		return nil, err
	}
	return membershipAsDocument(got), nil
}

func (t *Groups) ListMemberships(ctx context.Context, projectID, groupID string, q databases.Query, _ databases.Principal) ([]databases.Document, int64, string, error) {
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, 0, "", err
	}
	if _, err := t.getGroup(ctx, projectID, groupID); err != nil {
		return nil, 0, "", err
	}
	list, err := t.memberships.ListByGroup(ctx, projectID, groupID)
	if err != nil {
		return nil, 0, "", err
	}
	docs := make([]databases.Document, 0, len(list))
	for _, m := range list {
		if d := membershipAsDocument(m); d != nil {
			docs = append(docs, *d)
		}
	}
	return paginateDocuments(docs, q.PageSize, q.PageToken)
}

func (t *Groups) GetMembership(ctx context.Context, projectID, groupID, membershipID string, _ databases.Principal) (*databases.Document, error) {
	m, err := t.getMembership(ctx, projectID, groupID, membershipID)
	if err != nil {
		return nil, err
	}
	return membershipAsDocument(m), nil
}

func (t *Groups) UpdateMembership(ctx context.Context, projectID, groupID, membershipID string, cmd UpdateMembershipCommand, _ databases.Principal) (*databases.Document, error) {
	if err := groupsWriteGuard(ctx); err != nil {
		return nil, err
	}
	if len(cmd.Roles) == 0 {
		return nil, status.Error(codes.InvalidArgument, "roles is required")
	}
	for _, role := range cmd.Roles {
		if err := groups.ValidateRole(role); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	err := t.memberships.UpdateRoles(ctx, projectID, membershipID, func(txCtx context.Context, current *groups.Membership) ([]string, error) {
		if current.GroupID != groupID {
			return nil, status.Error(codes.NotFound, "membership not found")
		}
		if !containsRole(cmd.Roles, groups.RoleOwner) {
			if err := t.guardLastOwnerLocked(txCtx, projectID, groupID, current); err != nil {
				return nil, err
			}
		}
		return cmd.Roles, nil
	})
	if err != nil {
		if errors.Is(err, groups.ErrMembershipNotFound) {
			return nil, status.Error(codes.NotFound, "membership not found")
		}
		return nil, fmt.Errorf("update membership: %w", err)
	}
	return t.GetMembership(ctx, projectID, groupID, membershipID, databases.SystemPrincipal)
}

func (t *Groups) UpdateMembershipStatus(ctx context.Context, projectID, groupID, membershipID, statusVal string, _ databases.Principal) (*databases.Document, error) {
	if err := groupsWriteGuard(ctx); err != nil {
		return nil, err
	}
	if err := groups.ValidateStatus(statusVal); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if statusVal == groups.StatusPending {
		return nil, status.Error(codes.InvalidArgument, "cannot set status back to pending")
	}
	m, err := t.getMembership(ctx, projectID, groupID, membershipID)
	if err != nil {
		return nil, err
	}
	userID := m.UserID
	switch statusVal {
	case groups.StatusAccepted:
		if userID == "" {
			if m.Email == "" {
				return nil, status.Error(codes.FailedPrecondition, "membership has no user to accept")
			}
			userID, err = t.resolveUserIDByEmail(ctx, projectID, m.Email)
			if err != nil {
				return nil, err
			}
			if userID == "" {
				return nil, status.Error(codes.NotFound, "user not found for membership email")
			}
		}
		if err := t.memberships.Accept(ctx, projectID, membershipID, userID, time.Now()); err != nil {
			return nil, mapMembershipStatusError(err)
		}
	case groups.StatusRejected:
		if err := t.memberships.Reject(ctx, projectID, membershipID); err != nil {
			return nil, mapMembershipStatusError(err)
		}
	}
	return t.GetMembership(ctx, projectID, groupID, membershipID, databases.SystemPrincipal)
}

func (t *Groups) DeleteMembership(ctx context.Context, projectID, groupID, membershipID string, _ databases.Principal) error {
	if err := groupsWriteGuard(ctx); err != nil {
		return err
	}
	m, err := t.getMembership(ctx, projectID, groupID, membershipID)
	if err != nil {
		return err
	}
	if err := t.guardLastOwner(ctx, projectID, groupID, m); err != nil {
		return err
	}
	return t.memberships.Delete(ctx, projectID, membershipID)
}

func (t *Groups) ListAcceptedGroupRoles(ctx context.Context, projectID, userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	list, err := t.memberships.ListByUser(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range list {
		if m.Status != groups.StatusAccepted || m.GroupID == "" {
			continue
		}
		out = append(out, fmt.Sprintf("group:%s", m.GroupID), fmt.Sprintf("member:%s", m.ID))
	}
	return out, nil
}

func (t *Groups) AcceptedGroupRoleLabels(ctx context.Context, projectID, userID string) (map[string][]string, error) {
	if userID == "" {
		return nil, nil
	}
	list, err := t.memberships.ListByUser(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string)
	for _, m := range list {
		if m.Status != groups.StatusAccepted || m.GroupID == "" {
			continue
		}
		out[m.GroupID] = append([]string(nil), m.Roles...)
	}
	return out, nil
}

func (t *Groups) getMembership(ctx context.Context, projectID, groupID, membershipID string) (*groups.Membership, error) {
	if _, err := t.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	m, err := t.memberships.GetByID(ctx, projectID, membershipID)
	if err != nil {
		return nil, err
	}
	if m == nil || m.GroupID != groupID {
		return nil, status.Error(codes.NotFound, "membership not found")
	}
	return m, nil
}

func (t *Groups) guardLastOwner(ctx context.Context, projectID, groupID string, target *groups.Membership) error {
	list, err := t.memberships.ListByGroup(ctx, projectID, groupID)
	if err != nil {
		return err
	}
	return guardLastOwnerFromList(list, target)
}

func (t *Groups) guardLastOwnerLocked(ctx context.Context, projectID, groupID string, target *groups.Membership) error {
	list, err := t.memberships.ListByGroup(ctx, projectID, groupID)
	if err != nil {
		return err
	}
	return guardLastOwnerFromList(list, target)
}

func guardLastOwnerFromList(list []*groups.Membership, target *groups.Membership) error {
	if target.Status != groups.StatusAccepted || !containsRole(target.Roles, groups.RoleOwner) {
		return nil
	}
	otherOwners := 0
	for _, m := range list {
		if m.ID == target.ID {
			continue
		}
		if m.Status != groups.StatusAccepted {
			continue
		}
		if containsRole(m.Roles, groups.RoleOwner) {
			otherOwners++
		}
	}
	if otherOwners == 0 {
		return status.Error(codes.FailedPrecondition, "group must keep at least one owner")
	}
	return nil
}

func containsRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

func (t *Groups) resolveUserIDByEmail(ctx context.Context, projectID, email string) (string, error) {
	if t.usersRepo == nil {
		return "", status.Error(codes.Internal, "users repository is not configured")
	}
	found, err := t.usersRepo.GetByEmail(ctx, projectID, email)
	if err != nil {
		return "", err
	}
	if found == nil {
		return "", nil
	}
	return found.ID, nil
}

func mapMembershipStatusError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, groups.ErrMembershipNotPending) {
		return status.Error(codes.FailedPrecondition, "membership status is not pending")
	}
	if errors.Is(err, groups.ErrMembershipNotFound) {
		return status.Error(codes.NotFound, "membership not found")
	}
	if errors.Is(err, groups.ErrMembershipAlreadyExists) {
		return status.Error(codes.AlreadyExists, "membership already exists")
	}
	return err
}

func groupAsDocument(g *groups.Group) *databases.Document {
	if g == nil {
		return nil
	}
	prefs := g.Prefs
	if prefs == nil {
		prefs = map[string]any{}
	}
	perms := g.Permissions
	if perms == nil {
		perms = []string{}
	}
	return &databases.Document{
		ID:        g.ID,
		CreatedAt: g.CreatedAt,
		UpdatedAt: g.UpdatedAt,
		Data: map[string]any{
			"name":        g.Name,
			"permissions": perms,
			"total":       g.Total,
			"prefs":       prefs,
		},
	}
}

func membershipAsDocument(m *groups.Membership) *databases.Document {
	if m == nil {
		return nil
	}
	data := map[string]any{
		"group_id": m.GroupID,
		"user_id":  m.UserID,
		"email":    m.Email,
		"name":     m.Name,
		"roles":    m.Roles,
		"status":   m.Status,
	}
	if !m.InvitedAt.IsZero() {
		data["invited_at"] = m.InvitedAt
	}
	if !m.JoinedAt.IsZero() {
		data["joined_at"] = m.JoinedAt
	}
	return &databases.Document{
		ID:        m.ID,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
		Data:      data,
	}
}

func paginateDocuments(docs []databases.Document, pageSize int32, pageToken string) ([]databases.Document, int64, string, error) {
	total := int64(len(docs))
	offset := 0
	if pageToken != "" {
		var err error
		offset, err = crud.DecodePageToken(pageToken)
		if err != nil {
			return nil, 0, "", status.Error(codes.InvalidArgument, "invalid page_token")
		}
	}
	limit := int(pageSize)
	if limit <= 0 {
		limit = 25
	}
	if offset > len(docs) {
		offset = len(docs)
	}
	end := offset + limit
	if end > len(docs) {
		end = len(docs)
	}
	page := docs[offset:end]
	next := ""
	if end < len(docs) {
		token, err := crud.EncodePageToken(end)
		if err != nil {
			return nil, 0, "", status.Error(codes.Internal, err.Error())
		}
		next = token
	}
	return page, total, next, nil
}
