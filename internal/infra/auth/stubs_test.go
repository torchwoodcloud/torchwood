package auth_test

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
)

type stubUserRepo struct {
	mu    sync.Mutex
	users map[string]map[string]*domainusers.User
}

func newStubUserRepo() *stubUserRepo {
	return &stubUserRepo{users: map[string]map[string]*domainusers.User{}}
}

func (r *stubUserRepo) seed(projectID string, u *domainusers.User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.users[projectID] == nil {
		r.users[projectID] = map[string]*domainusers.User{}
	}
	cp := *u
	r.users[projectID][u.ID] = &cp
}

func (r *stubUserRepo) GetByID(_ context.Context, projectID, id string) (*domainusers.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[projectID][id]
	if u == nil {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}
func (r *stubUserRepo) GetByEmail(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *stubUserRepo) GetByPhone(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *stubUserRepo) Insert(context.Context, string, *domainusers.User) error { return nil }
func (r *stubUserRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *stubUserRepo) Delete(context.Context, string, string) error { return nil }
func (r *stubUserRepo) List(context.Context, string, domainusers.ListFilter) (*domainusers.ListResult, error) {
	return &domainusers.ListResult{}, nil
}
func (r *stubUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return nil
}

type stubSessionRepo struct {
	mu       sync.Mutex
	sessions map[string]map[string]*domainauth.Session
}

func newStubSessionRepo() *stubSessionRepo {
	return &stubSessionRepo{sessions: map[string]map[string]*domainauth.Session{}}
}

func (r *stubSessionRepo) seed(projectID string, s *domainauth.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[projectID] == nil {
		r.sessions[projectID] = map[string]*domainauth.Session{}
	}
	cp := *s
	r.sessions[projectID][s.ID] = &cp
}

func (r *stubSessionRepo) Insert(_ context.Context, projectID string, s *domainauth.Session) error {
	r.seed(projectID, s)
	return nil
}

func (r *stubSessionRepo) GetByID(_ context.Context, projectID, id string) (*domainauth.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[projectID][id]
	if s == nil {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (r *stubSessionRepo) ListByUser(_ context.Context, projectID, userID string) ([]domainauth.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domainauth.Session
	for _, s := range r.sessions[projectID] {
		if s.UserID == userID {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (r *stubSessionRepo) Delete(_ context.Context, projectID, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions[projectID], id)
	return nil
}

func (r *stubSessionRepo) DeleteByUser(_ context.Context, projectID, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.sessions[projectID] {
		if s.UserID == userID {
			delete(r.sessions[projectID], id)
		}
	}
	return nil
}

func (r *stubSessionRepo) DeleteOldestByUser(_ context.Context, projectID, userID string, keep int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	type item struct {
		id string
		t  time.Time
	}
	var items []item
	for _, s := range r.sessions[projectID] {
		if s.UserID == userID {
			items = append(items, item{s.ID, s.ExpireAt})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].t.Before(items[j].t) })
	drop := len(items) - keep
	if drop <= 0 {
		return nil
	}
	for i := 0; i < drop; i++ {
		delete(r.sessions[projectID], items[i].id)
	}
	return nil
}

func (r *stubSessionRepo) len(projectID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions[projectID])
}

func (r *stubSessionRepo) ids(projectID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for id := range r.sessions[projectID] {
		out = append(out, id)
	}
	return out
}

func (r *stubSessionRepo) get(projectID, id string) *domainauth.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[projectID][id]
}

func activeUser(id string) *domainusers.User {
	return &domainusers.User{ID: id, Status: domainusers.StatusActive}
}
