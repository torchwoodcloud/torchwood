package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/pkg/password"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUsers_CreateUser_UsesUserRepository(t *testing.T) {
	t.Parallel()

	repo := newRecordingUserRepo()
	uc := NewUsers(fakeProjectRepo{}, nil, nil, repo, nil, nil, nil)
	ctx := platformAdminCtx(context.Background())

	doc, err := uc.CreateUser(ctx, "proj-1", CreateUserCommand{
		Email:    "Server@Torchwood.local",
		Password: "Passw0rd",
		Name:     "Srv",
		Labels:   []any{"vip"},
		Prefs:    map[string]any{"theme": "dark"},
	})
	require.NoError(t, err)
	require.NotNil(t, doc)
	require.Equal(t, "server@torchwood.local", doc.Data["email"])
	require.Equal(t, users.StatusActive, doc.Data["status"])
	require.Equal(t, false, doc.Data["email_verified"])
	require.Equal(t, []any{"vip"}, doc.Data["labels"])
	require.Equal(t, map[string]any{"theme": "dark"}, doc.Data["prefs"])
	// 密码经 repo 哈希存储（fake repo 读回验证）；投影响应面不含 password_hash。
	require.NotContains(t, doc.Data, "password_hash")
	require.Equal(t, []string{"GetByEmail", "Insert"}, repo.calls)
	stored, err := repo.GetByID(ctx, "proj-1", doc.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	ok, err := password.Verify("Passw0rd", stored.PasswordHash)
	require.NoError(t, err)
	require.True(t, ok)

	repo.calls = nil
	_, err = uc.CreateUser(ctx, "proj-1", CreateUserCommand{
		Email:    "server@torchwood.local",
		Password: "Passw0rd",
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.Equal(t, []string{"GetByEmail"}, repo.calls)
}

type recordingUserRepo struct {
	calls   []string
	byEmail map[string]*users.User
	byID    map[string]*users.User
}

func newRecordingUserRepo() *recordingUserRepo {
	return &recordingUserRepo{
		byEmail: map[string]*users.User{},
		byID:    map[string]*users.User{},
	}
}

func (r *recordingUserRepo) GetByEmail(_ context.Context, _, email string) (*users.User, error) {
	r.calls = append(r.calls, "GetByEmail")
	return r.byEmail[users.NormalizeEmail(email)], nil
}

func (r *recordingUserRepo) GetByID(_ context.Context, _, id string) (*users.User, error) {
	r.calls = append(r.calls, "GetByID")
	return r.byID[id], nil
}

func (r *recordingUserRepo) GetByPhone(context.Context, string, string) (*users.User, error) {
	r.calls = append(r.calls, "GetByPhone")
	return nil, nil
}

func (r *recordingUserRepo) Insert(_ context.Context, _ string, user *users.User) error {
	r.calls = append(r.calls, "Insert")
	if user == nil {
		return users.ErrUserIDRequired
	}
	if r.byEmail[user.Email] != nil {
		return users.ErrEmailAlreadyRegistered
	}
	cloned := *user
	r.byEmail[user.Email] = &cloned
	r.byID[user.ID] = &cloned
	return nil
}

func (r *recordingUserRepo) Update(context.Context, string, string, map[string]any) error {
	r.calls = append(r.calls, "Update")
	return nil
}

func (r *recordingUserRepo) Delete(context.Context, string, string) error {
	r.calls = append(r.calls, "Delete")
	return nil
}

func (r *recordingUserRepo) List(context.Context, string, users.ListFilter) (*users.ListResult, error) {
	r.calls = append(r.calls, "List")
	return &users.ListResult{}, nil
}

func (r *recordingUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	r.calls = append(r.calls, "UpdateFactors")
	return nil
}

var _ users.Repository = (*recordingUserRepo)(nil)

type usersCollectionGuardDocDB struct{}

func (usersCollectionGuardDocDB) ListDocuments(_ context.Context, _, _, collectionID string, _ databases.Query, _ databases.Principal) (*databases.DocumentList, error) {
	if collectionID == users.CollectionID {
		panic("ListDocuments(users) forbidden: use UserRepository.GetByEmail")
	}
	return &databases.DocumentList{}, nil
}
func (usersCollectionGuardDocDB) CreateDocument(_ context.Context, _, _, collectionID string, doc databases.Document, _ []databases.Permission, _ databases.Principal) (databases.Document, error) {
	if collectionID == users.CollectionID {
		panic("CreateDocument(users) forbidden: use UserRepository.Insert")
	}
	return doc, nil
}
func (usersCollectionGuardDocDB) EnsureCatalog(context.Context, string) error { return nil }
func (usersCollectionGuardDocDB) CreateDatabase(context.Context, string, string, string) error {
	return nil
}
func (usersCollectionGuardDocDB) GetDatabase(context.Context, string, string) (*databases.Database, error) {
	return nil, nil
}
func (usersCollectionGuardDocDB) ListDatabases(context.Context, string) ([]databases.Database, error) {
	return nil, nil
}
func (usersCollectionGuardDocDB) DeleteDatabase(context.Context, string, string) error { return nil }
func (usersCollectionGuardDocDB) CreateCollection(context.Context, string, string, string, string, []databases.Attribute, []databases.Index, []databases.Permission, bool) error {
	return nil
}
func (usersCollectionGuardDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return nil, nil
}
func (usersCollectionGuardDocDB) ListCollections(context.Context, string, string, databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (usersCollectionGuardDocDB) DeleteCollection(context.Context, string, string, string) error {
	return nil
}
func (usersCollectionGuardDocDB) UpdateCollection(context.Context, string, string, string, databases.CollectionPatch) error {
	return nil
}
func (usersCollectionGuardDocDB) CreateAttribute(context.Context, string, string, string, databases.Attribute) error {
	return nil
}
func (usersCollectionGuardDocDB) DeleteAttribute(context.Context, string, string, string, string) error {
	return nil
}
func (usersCollectionGuardDocDB) CreateIndex(context.Context, string, string, string, databases.Index) error {
	return nil
}
func (usersCollectionGuardDocDB) DeleteIndex(context.Context, string, string, string, string) error {
	return nil
}
func (usersCollectionGuardDocDB) UpsertDocument(context.Context, string, string, string, databases.Document, []string, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (usersCollectionGuardDocDB) GetDocument(context.Context, string, string, string, string, databases.Principal) (*databases.Document, error) {
	return nil, nil
}
func (usersCollectionGuardDocDB) UpdateDocument(context.Context, string, string, string, databases.DocumentUpdate, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (usersCollectionGuardDocDB) DeleteDocument(context.Context, string, string, string, string, databases.DeleteOptions, databases.Principal) error {
	return nil
}
func (usersCollectionGuardDocDB) CountDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (int64, error) {
	return 0, nil
}
func (usersCollectionGuardDocDB) AggregateDocuments(context.Context, string, string, string, databases.Query, []databases.AggregateSpec, string, databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (usersCollectionGuardDocDB) BulkUpdateDocuments(context.Context, string, string, string, []string, map[string]any, []databases.Permission, databases.Principal) (int64, error) {
	return 0, nil
}
func (usersCollectionGuardDocDB) BulkDeleteDocuments(context.Context, string, string, string, []string, databases.Principal) (int64, error) {
	return 0, nil
}

func (usersCollectionGuardDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (usersCollectionGuardDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	panic("ListChanges: not implemented in test fake")
}

var _ databases.DocumentDB = usersCollectionGuardDocDB{}
