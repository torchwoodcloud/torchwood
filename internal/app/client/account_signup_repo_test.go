package client

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// allowRateLimiter 是始终放行的限流器桩（匿名会话 fail-closed 后测试注入）。
type allowRateLimiter struct{}

func (allowRateLimiter) Allow(context.Context, string, int, time.Duration) error { return nil }

// 证明 SignUp 走 UserRepository.GetByEmail / Insert / User.Register，
// 而不是 ListDocuments + query.BuildEqual("email") 薄包装。
func TestAccount_SignUp_UsesUserRepository(t *testing.T) {
	t.Parallel()

	repo := newRecordingUserRepo()
	account := newAccountWithUserRepo(repo, "proj-1")

	user, tokens, cookie, _, err := account.SignUp(context.Background(), SignUpCommand{
		ProjectID: "proj-1",
		Email:     "SignUp@Torchwood.local",
		Password:  "Passw0rd",
		Name:      "Pat",
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, "signup@torchwood.local", user.Email)
	require.Equal(t, "Pat", user.Name)
	require.NotEmpty(t, user.ID)
	require.NotNil(t, tokens)
	require.Equal(t, "at", tokens.AccessToken)
	require.Equal(t, "cookie", cookie)
	require.Equal(t, []string{"GetByEmail", "Insert"}, repo.calls)
	require.NotNil(t, repo.byID[user.ID])
	require.NotEmpty(t, repo.byID[user.ID].PasswordHash)
	require.False(t, repo.byID[user.ID].IsAnonymous())

	repo.calls = nil
	_, _, _, _, err = account.SignUp(context.Background(), SignUpCommand{
		ProjectID: "proj-1",
		Email:     "signup@torchwood.local",
		Password:  "Passw0rd",
		Name:      "Dup",
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.Equal(t, []string{"GetByEmail"}, repo.calls)
}

func TestAccount_findOrCreateUserByEmail_UsesUserRepository(t *testing.T) {
	t.Parallel()

	repo := newRecordingUserRepo()
	account := newAccountWithUserRepo(repo, "proj-1")

	user, err := account.findOrCreateUserByEmail(context.Background(), "proj-1", "otp@torchwood.local", true)
	require.NoError(t, err)
	require.Equal(t, "otp@torchwood.local", user.Email)
	require.True(t, user.EmailVerified)
	require.Equal(t, []string{"GetByEmail", "Insert"}, repo.calls)
	require.Empty(t, repo.byID[user.ID].PasswordHash)

	repo.calls = nil
	again, err := account.findOrCreateUserByEmail(context.Background(), "proj-1", "otp@torchwood.local", true)
	require.NoError(t, err)
	require.Equal(t, user.ID, again.ID)
	require.Equal(t, []string{"GetByEmail"}, repo.calls)
}

func TestAccount_findOrCreateUserByPhone_UsesUserRepository(t *testing.T) {
	t.Parallel()

	repo := newRecordingUserRepo()
	account := newAccountWithUserRepo(repo, "proj-1")

	user, err := account.findOrCreateUserByPhone(context.Background(), "proj-1", "+15551234567")
	require.NoError(t, err)
	require.Equal(t, "+15551234567", user.Name)
	require.Equal(t, []string{"GetByPhone", "Insert"}, repo.calls)
	stored := repo.byID[user.ID]
	require.NotNil(t, stored)
	require.Equal(t, "+15551234567", stored.Phone)
	require.True(t, stored.PhoneVerified)
	require.Empty(t, stored.PasswordHash)
	require.Equal(t, phonePlaceholderEmail("+15551234567"), stored.Email)

	repo.calls = nil
	again, err := account.findOrCreateUserByPhone(context.Background(), "proj-1", "+15551234567")
	require.NoError(t, err)
	require.Equal(t, user.ID, again.ID)
	require.Equal(t, []string{"GetByPhone"}, repo.calls)
}

func TestAccount_CreateAnonymousSession_UsesUserRepository(t *testing.T) {
	t.Parallel()

	repo := newRecordingUserRepo()
	account := newAccountWithUserRepo(repo, "proj-1")

	// M5 C8：匿名会话限流 fail-closed，测试注入允许型限流器与 ClientInfo。
	account.rateLimiter = allowRateLimiter{}
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.9"})
	user, _, _, _, err := account.CreateAnonymousSession(ctx, CreateAnonymousSessionCommand{ProjectID: "proj-1"})
	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, []string{"Insert"}, repo.calls)
	stored := repo.byID[user.ID]
	require.NotNil(t, stored)
	require.True(t, stored.IsAnonymous())
	require.Empty(t, stored.PasswordHash)
}

func newAccountWithUserRepo(repo users.Repository, projectID string) *Account {
	return NewAccount(
		&config.AppConfig{},
		signupProjectRepo{id: projectID},
		nil,
		nil,
		stubSessionService{},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		repo,
		nil,
		nil,
		nil, nil, nil,
		nil,
	)
}

type recordingUserRepo struct {
	calls   []string
	byEmail map[string]*users.User
	byID    map[string]*users.User
	byPhone map[string]*users.User
}

func newRecordingUserRepo() *recordingUserRepo {
	return &recordingUserRepo{
		byEmail: map[string]*users.User{},
		byID:    map[string]*users.User{},
		byPhone: map[string]*users.User{},
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

func (r *recordingUserRepo) GetByPhone(_ context.Context, _, phone string) (*users.User, error) {
	r.calls = append(r.calls, "GetByPhone")
	return r.byPhone[phone], nil
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
	if user.Phone != "" {
		r.byPhone[user.Phone] = &cloned
	}
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

type signupProjectRepo struct{ id string }

func (r signupProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	if id != r.id {
		return nil, nil
	}
	return &projects.Project{ID: r.id, InternalID: 1, Status: "active"}, nil
}
func (signupProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (signupProjectRepo) ListProjects(context.Context) ([]projects.Project, error) { return nil, nil }
func (signupProjectRepo) CreateProject(context.Context, *projects.Project) error   { return nil }
func (signupProjectRepo) UpdateProject(context.Context, *projects.Project) error   { return nil }
func (signupProjectRepo) DeleteProject(context.Context, string) error              { return nil }

type stubSessionService struct{}

func (stubSessionService) CreateSessionAndTokens(_ context.Context, _, _, _, _ string) (*domainauth.TokenBundle, string, error) {
	return &domainauth.TokenBundle{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}, "cookie", nil
}
func (stubSessionService) CreateShortLivedSessionAndTokens(_ context.Context, _, _, _, _ string) (*domainauth.TokenBundle, string, error) {
	return &domainauth.TokenBundle{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}, "cookie", nil
}
func (stubSessionService) IssueTokens(context.Context, string, string, string, string) (*domainauth.TokenBundle, string, error) {
	return nil, "", nil
}
func (stubSessionService) IssueTokensWithRefreshID(context.Context, string, string, string, string, string) (*domainauth.TokenBundle, string, error) {
	return nil, "", nil
}
func (stubSessionService) EnsureActiveSession(context.Context, string, string, string) error {
	return nil
}
func (stubSessionService) DeleteSessionsByUser(context.Context, string, string) error { return nil }

// usersCollectionGuardDocDB 在 users 集合上 List/Create 直接 panic，防止用例走文档 DSL 换皮。
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

func (r signupProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }
