package servergrpc

import (
	"context"
	"encoding/json"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domaingroups "github.com/torchwoodcloud/torchwood/internal/domain/groups"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
)

type paginationUserRepo struct{}

func (paginationUserRepo) GetByEmail(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (paginationUserRepo) GetByID(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (paginationUserRepo) GetByPhone(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (paginationUserRepo) Insert(context.Context, string, *domainusers.User) error { return nil }
func (paginationUserRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (paginationUserRepo) Delete(context.Context, string, string) error { return nil }
func (paginationUserRepo) List(context.Context, string, domainusers.ListFilter) (*domainusers.ListResult, error) {
	return &domainusers.ListResult{
		Users:         []*domainusers.User{{ID: "u-1", Email: "a@b.c", Status: domainusers.StatusActive}},
		TotalCount:    1,
		NextPageToken: "tok-1",
	}, nil
}
func (paginationUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return nil
}

type paginationGroupRepo struct{}

func (paginationGroupRepo) Insert(context.Context, string, *domaingroups.Group) error { return nil }
func (paginationGroupRepo) GetByID(_ context.Context, _, id string) (*domaingroups.Group, error) {
	return &domaingroups.Group{ID: id, Name: "T", Total: 1}, nil
}
func (paginationGroupRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (paginationGroupRepo) Delete(context.Context, string, string) error { return nil }
func (paginationGroupRepo) List(context.Context, string) ([]*domaingroups.Group, error) {
	return []*domaingroups.Group{
		{ID: "group-1", Name: "T", Total: 1},
		{ID: "group-2", Name: "U", Total: 0},
	}, nil
}
func (paginationGroupRepo) AddTotal(context.Context, string, string, int64) error { return nil }
func (paginationGroupRepo) RecountAccepted(context.Context, string, string) error {
	return nil
}

type paginationMembershipRepo struct{}

func (paginationMembershipRepo) Insert(context.Context, string, *domaingroups.Membership) error {
	return nil
}
func (paginationMembershipRepo) GetByID(context.Context, string, string) (*domaingroups.Membership, error) {
	return nil, nil
}
func (paginationMembershipRepo) ListByGroup(context.Context, string, string) ([]*domaingroups.Membership, error) {
	return []*domaingroups.Membership{
		{ID: "m-1", GroupID: "group-1", UserID: "u-1", Status: domaingroups.StatusAccepted},
		{ID: "m-2", GroupID: "group-1", UserID: "u-2", Status: domaingroups.StatusAccepted},
	}, nil
}
func (paginationMembershipRepo) ListByUser(context.Context, string, string) ([]*domaingroups.Membership, error) {
	return nil, nil
}
func (paginationMembershipRepo) Delete(context.Context, string, string) error { return nil }
func (paginationMembershipRepo) Accept(context.Context, string, string, string, time.Time) error {
	return nil
}
func (paginationMembershipRepo) Reject(context.Context, string, string) error { return nil }
func (paginationMembershipRepo) UpdateRoles(context.Context, string, string, func(context.Context, *domaingroups.Membership) ([]string, error)) error {
	return nil
}

type paginationBucketRepo struct{}

func (paginationBucketRepo) Insert(context.Context, string, *domainstorage.Bucket) error { return nil }
func (paginationBucketRepo) GetByID(_ context.Context, _, id string) (*domainstorage.Bucket, error) {
	if id == "b-1" {
		return &domainstorage.Bucket{ID: "b-1", Name: "B", Public: true, Permissions: []string{"read"}}, nil
	}
	return nil, nil
}
func (paginationBucketRepo) List(context.Context, string) ([]*domainstorage.Bucket, error) {
	return []*domainstorage.Bucket{
		{ID: "b-1", Name: "B", Permissions: []string{"read"}},
		{ID: "b-2", Name: "C"},
	}, nil
}
func (paginationBucketRepo) Count(context.Context, string) (int64, error) { return 2, nil }
func (paginationBucketRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (paginationBucketRepo) Delete(context.Context, string, string) error { return nil }

type paginationFileRepo struct{}

func (paginationFileRepo) Insert(context.Context, string, *domainstorage.File) error { return nil }
func (paginationFileRepo) GetByID(context.Context, string, string) (*domainstorage.File, error) {
	return nil, nil
}
func (paginationFileRepo) ListByBucket(context.Context, string, string) ([]*domainstorage.File, error) {
	return []*domainstorage.File{
		{ID: "f-1", BucketID: "b-1", Name: "x.png", MimeType: "image/png", Size: 3},
		{ID: "f-2", BucketID: "b-1", Name: "y.png", MimeType: "image/png", Size: 1},
	}, nil
}
func (paginationFileRepo) Count(context.Context, string) (int64, error) { return 2, nil }
func (paginationFileRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (paginationFileRepo) Delete(context.Context, string, string) error { return nil }
func (paginationFileRepo) SumSize(context.Context, string) (int64, error) {
	return 0, nil
}

var _ domainauth.SessionRepository = paginationSessionRepo{}

type paginationSessionRepo struct{}

func (paginationSessionRepo) Insert(context.Context, string, *domainauth.Session) error { return nil }
func (paginationSessionRepo) GetByID(context.Context, string, string) (*domainauth.Session, error) {
	return nil, nil
}
func (paginationSessionRepo) ListByUser(context.Context, string, string) ([]domainauth.Session, error) {
	return nil, nil
}
func (paginationSessionRepo) Delete(context.Context, string, string) error { return nil }
func (paginationSessionRepo) DeleteByUser(context.Context, string, string) error {
	return nil
}
func (paginationSessionRepo) DeleteOldestByUser(context.Context, string, string, int) error {
	return nil
}
