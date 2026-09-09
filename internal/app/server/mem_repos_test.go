package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domaingroups "github.com/torchwoodcloud/torchwood/internal/domain/groups"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type memUserRepo struct {
	mu    sync.Mutex
	users map[string]*domainusers.User
}

func newMemUserRepo() *memUserRepo {
	return &memUserRepo{users: map[string]*domainusers.User{}}
}

func (r *memUserRepo) seed(u *domainusers.User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *u
	r.users[u.ID] = &cp
}

func (r *memUserRepo) GetByEmail(_ context.Context, _, email string) (*domainusers.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	email = domainusers.NormalizeEmail(email)
	for _, u := range r.users {
		if u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *memUserRepo) GetByID(_ context.Context, _, id string) (*domainusers.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[id]
	if u == nil {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}

func (r *memUserRepo) GetByPhone(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *memUserRepo) Insert(context.Context, string, *domainusers.User) error { return nil }
func (r *memUserRepo) Update(_ context.Context, _, id string, cols map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[id]
	if u == nil {
		return status.Error(codes.NotFound, "user not found")
	}
	if email, ok := cols["email"].(string); ok {
		norm := domainusers.NormalizeEmail(email)
		for _, other := range r.users {
			if other.ID != id && other.Email == norm {
				return status.Error(codes.AlreadyExists, "email already registered")
			}
		}
		u.Email = norm
	}
	return nil
}
func (r *memUserRepo) Delete(context.Context, string, string) error { return nil }
func (r *memUserRepo) List(context.Context, string, domainusers.ListFilter) (*domainusers.ListResult, error) {
	return &domainusers.ListResult{}, nil
}
func (r *memUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return nil
}

type memGroupRepo struct {
	mu     sync.Mutex
	groups map[string]*domaingroups.Group
}

func newMemGroupRepo() *memGroupRepo {
	return &memGroupRepo{groups: map[string]*domaingroups.Group{}}
}

func (r *memGroupRepo) seed(g *domaingroups.Group) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *g
	r.groups[g.ID] = &cp
}

func (r *memGroupRepo) Insert(_ context.Context, _ string, g *domaingroups.Group) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *g
	r.groups[g.ID] = &cp
	return nil
}
func (r *memGroupRepo) GetByID(_ context.Context, _, id string) (*domaingroups.Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.groups[id]
	if g == nil {
		return nil, nil
	}
	cp := *g
	return &cp, nil
}
func (r *memGroupRepo) Update(context.Context, string, string, map[string]any) error { return nil }
func (r *memGroupRepo) Delete(_ context.Context, _, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.groups, id)
	return nil
}
func (r *memGroupRepo) List(context.Context, string) ([]*domaingroups.Group, error) { return nil, nil }
func (r *memGroupRepo) AddTotal(_ context.Context, _, groupID string, delta int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.groups[groupID]
	if g == nil {
		g = &domaingroups.Group{ID: groupID}
		r.groups[groupID] = g
	}
	g.Total += delta
	if g.Total < 0 {
		g.Total = 0
	}
	return nil
}
func (r *memGroupRepo) RecountAccepted(context.Context, string, string) error { return nil }

type memMembershipRepo struct {
	mu     sync.Mutex
	rows   map[string]*domaingroups.Membership
	groups *memGroupRepo
}

func newMemMembershipRepo() *memMembershipRepo {
	return &memMembershipRepo{rows: map[string]*domaingroups.Membership{}}
}

func (r *memMembershipRepo) seed(m *domaingroups.Membership) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *m
	r.rows[m.ID] = &cp
}

func (r *memMembershipRepo) Insert(_ context.Context, _ string, m *domaingroups.Membership) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.rows {
		if existing.GroupID != m.GroupID {
			continue
		}
		if m.UserID != "" && existing.UserID == m.UserID {
			return domaingroups.ErrMembershipAlreadyExists
		}
		if m.Email != "" && existing.Email == m.Email {
			return domaingroups.ErrMembershipAlreadyExists
		}
	}
	cp := *m
	r.rows[m.ID] = &cp
	if m.Status == domaingroups.StatusAccepted && r.groups != nil {
		_ = r.groups.AddTotal(context.Background(), "", m.GroupID, 1)
	}
	return nil
}

func (r *memMembershipRepo) GetByID(_ context.Context, _, id string) (*domaingroups.Membership, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.rows[id]
	if m == nil {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

func (r *memMembershipRepo) ListByGroup(_ context.Context, _, groupID string) ([]*domaingroups.Membership, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domaingroups.Membership
	for _, m := range r.rows {
		if m.GroupID == groupID {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *memMembershipRepo) ListByUser(_ context.Context, _, userID string) ([]*domaingroups.Membership, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domaingroups.Membership
	for _, m := range r.rows {
		if m.UserID == userID {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *memMembershipRepo) Delete(_ context.Context, _, id string) error {
	r.mu.Lock()
	m := r.rows[id]
	delete(r.rows, id)
	r.mu.Unlock()
	if m != nil && m.Status == domaingroups.StatusAccepted && r.groups != nil {
		_ = r.groups.AddTotal(context.Background(), "", m.GroupID, -1)
	}
	return nil
}

func (r *memMembershipRepo) Accept(_ context.Context, _, id, userID string, joinedAt time.Time) error {
	r.mu.Lock()
	m := r.rows[id]
	if m == nil {
		r.mu.Unlock()
		return domaingroups.ErrMembershipNotFound
	}
	if m.Status != domaingroups.StatusPending {
		r.mu.Unlock()
		return domaingroups.ErrMembershipNotPending
	}
	m.Status = domaingroups.StatusAccepted
	m.UserID = userID
	m.JoinedAt = joinedAt
	groupID := m.GroupID
	r.mu.Unlock()
	if r.groups != nil {
		_ = r.groups.AddTotal(context.Background(), "", groupID, 1)
	}
	return nil
}

func (r *memMembershipRepo) Reject(_ context.Context, _, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.rows[id]
	if m == nil {
		return domaingroups.ErrMembershipNotFound
	}
	if m.Status != domaingroups.StatusPending {
		return domaingroups.ErrMembershipNotPending
	}
	m.Status = domaingroups.StatusRejected
	return nil
}

func (r *memMembershipRepo) UpdateRoles(ctx context.Context, projectID, id string, mutate func(ctx context.Context, current *domaingroups.Membership) ([]string, error)) error {
	r.mu.Lock()
	m := r.rows[id]
	if m == nil {
		r.mu.Unlock()
		return domaingroups.ErrMembershipNotFound
	}
	cp := *m
	r.mu.Unlock()
	roles, err := mutate(ctx, &cp)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m.Roles = roles
	return nil
}

func (r *memMembershipRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

type memSessionRepo struct {
	mu       sync.Mutex
	sessions map[string]*domainauth.Session
}

func newMemSessionRepo() *memSessionRepo {
	return &memSessionRepo{sessions: map[string]*domainauth.Session{}}
}

func (r *memSessionRepo) Insert(_ context.Context, _ string, s *domainauth.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *s
	r.sessions[s.ID] = &cp
	return nil
}
func (r *memSessionRepo) GetByID(_ context.Context, _, id string) (*domainauth.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[id]
	if s == nil {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}
func (r *memSessionRepo) ListByUser(_ context.Context, _, userID string) ([]domainauth.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domainauth.Session
	for _, s := range r.sessions {
		if s.UserID == userID {
			out = append(out, *s)
		}
	}
	return out, nil
}
func (r *memSessionRepo) Delete(_ context.Context, _, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
	return nil
}
func (r *memSessionRepo) DeleteByUser(_ context.Context, _, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.sessions {
		if s.UserID == userID {
			delete(r.sessions, id)
		}
	}
	return nil
}
func (r *memSessionRepo) DeleteOldestByUser(_ context.Context, _, userID string, keep int) error {
	list, _ := r.ListByUser(context.Background(), "", userID)
	if len(list) <= keep {
		return nil
	}
	// naive: drop extras by expire_at
	type pair struct {
		id string
		t  time.Time
	}
	var items []pair
	r.mu.Lock()
	for _, s := range r.sessions {
		if s.UserID == userID {
			items = append(items, pair{s.ID, s.ExpireAt})
		}
	}
	r.mu.Unlock()
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].t.Before(items[i].t) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	drop := len(items) - keep
	for i := 0; i < drop; i++ {
		_ = r.Delete(context.Background(), "", items[i].id)
	}
	return nil
}
