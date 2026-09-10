package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/documents"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

func TestNewDatabases_HoldsSharedDocumentsCore(t *testing.T) {
	d := NewDatabases(stubProjects{}, &stubDocDB{}, nil)
	require.NotNil(t, d.docs)
}

func TestCreateDocument_GoesThroughSharedCoreWithOwnerACE(t *testing.T) {
	catalog := &stubDocDB{coll: &databases.Collection{ID: "notes"}}
	rec := &stubDocDB{}
	d := &Databases{
		projectRepo: stubProjects{},
		docDB:       catalog,
		docs:        documents.New(rec, nil),
	}
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: "p1",
		UserID:    "u1",
		Roles:     []string{"users", "user:u1"},
	})
	created, _, err := d.CreateDocument(ctx, "app", "notes", "d1", map[string]any{"t": 1}, nil, "")
	require.NoError(t, err)
	require.Equal(t, "d1", created.ID)
	require.Equal(t, 1, rec.creates)
	require.Zero(t, catalog.creates)
	require.Equal(t, ownerDocumentPermissions("u1"), rec.lastCreatePerms)
}

func TestResolveProject_PassesPlatformAdmin(t *testing.T) {
	d := NewDatabases(stubProjects{}, &stubDocDB{}, nil)
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind:       shared.ActorKindEndUser,
		ProjectID:       "p1",
		UserID:          "u1",
		Roles:           []string{"users", "user:u1"},
		IsPlatformAdmin: true,
	})
	_, principal, err := d.resolveProject(ctx)
	require.NoError(t, err)
	require.True(t, principal.PlatformAdmin)
	require.Equal(t, []string{"users", "user:u1"}, principal.Roles)
}

func TestResolveProject_AdminWithoutUserID(t *testing.T) {
	d := NewDatabases(stubProjects{}, &stubDocDB{}, nil)
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind:       shared.ActorKindAdmin,
		AdminID:         "a1",
		ProjectID:       "p1",
		Roles:           []string{"member", shared.RoleConsole},
		IsPlatformAdmin: true,
	})
	_, _, err := d.resolveProject(ctx)
	// C2：Client 写路径仅允许 EndUser，Admin 直调应被拒。
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthenticated")
}

func TestResolveReadPrincipal_AdminNotGuest(t *testing.T) {
	rec := &stubDocDB{coll: &databases.Collection{ID: "posts"}, list: &databases.DocumentList{}}
	d := &Databases{
		projectRepo: stubProjects{},
		docDB:       rec,
		docs:        documents.New(rec, nil),
	}
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind:       shared.ActorKindAdmin,
		AdminID:         "a1",
		ProjectID:       "p1",
		Roles:           []string{"member", shared.RoleConsole},
		IsPlatformAdmin: true,
	})
	_, err := d.ListDocuments(ctx, "p1", "app", "posts", databases.Query{})
	require.NoError(t, err)
	require.True(t, rec.lastListPrincipal.PlatformAdmin)
	require.NotEqual(t, databases.GuestPrincipal, rec.lastListPrincipal)
}

func TestListDocuments_GuestPrincipal(t *testing.T) {
	catalog := &stubDocDB{coll: &databases.Collection{ID: "posts"}}
	rec := &stubDocDB{list: &databases.DocumentList{}}
	d := &Databases{
		projectRepo: stubProjects{},
		docDB:       catalog,
		docs:        documents.New(rec, nil),
	}
	_, err := d.ListDocuments(context.Background(), "p1", "app", "posts", databases.Query{})
	require.NoError(t, err)
	require.Equal(t, databases.GuestPrincipal, rec.lastListPrincipal)
}

func TestUpdateDocument_FiltersProtectedFields(t *testing.T) {
	catalog := &stubDocDB{coll: &databases.Collection{ID: "notes"}}
	rec := &stubDocDB{}
	rec.docs = map[string]databases.Document{"d1": {ID: "d1", Version: 1, Data: map[string]any{"title": "old"}}}
	d := &Databases{
		projectRepo: stubProjects{},
		docDB:       catalog,
		docs:        documents.New(rec, nil),
	}
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: "p1",
		UserID:    "u1",
		Roles:     []string{"users", "user:u1"},
	})
	version := int64(1)
	_, _, err := d.UpdateDocument(ctx, "app", "notes", "d1", map[string]any{
		"title":          "new",
		"password_hash":  "evil",
		"email_verified": true,
		"labels":         []string{"admin"},
		"status":         "blocked",
	}, nil, nil, nil, &version, "")
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"title":          "new",
		"password_hash":  "evil",
		"email_verified": true,
		"labels":         []string{"admin"},
		"status":         "blocked",
	}, rec.lastUpdateData)
}

type stubProjects struct{}

func (stubProjects) CreateProject(context.Context, *projects.Project) error { return nil }
func (stubProjects) GetProject(_ context.Context, id string) (*projects.Project, error) {
	return &projects.Project{ID: id, InternalID: 1}, nil
}
func (stubProjects) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (stubProjects) ListProjects(context.Context) ([]projects.Project, error) { return nil, nil }
func (stubProjects) UpdateProject(context.Context, *projects.Project) error   { return nil }
func (stubProjects) DeleteProject(context.Context, string) error              { return nil }

type stubDocDB struct {
	coll              *databases.Collection
	docs              map[string]databases.Document
	list              *databases.DocumentList
	creates           int
	lastCreatePerms   []databases.Permission
	lastUpdateData    map[string]any
	lastListPrincipal databases.Principal
}

func (s *stubDocDB) CreateDocument(_ context.Context, _, _, _ string, doc databases.Document, perms []databases.Permission, _ databases.Principal) (databases.Document, error) {
	s.creates++
	s.lastCreatePerms = append([]databases.Permission{}, perms...)
	if doc.ID == "" {
		doc.ID = "generated"
	}
	doc.Version = 1
	return doc, nil
}

func (s *stubDocDB) UpdateDocument(_ context.Context, _, _, _ string, update databases.DocumentUpdate, _ databases.Principal) (databases.Document, error) {
	s.lastUpdateData = update.Document.Data
	doc := update.Document
	doc.Version = update.ExpectedVersion + 1
	return doc, nil
}

func (s *stubDocDB) GetDocument(_ context.Context, _, _, _, docID string, _ databases.Principal) (*databases.Document, error) {
	if s.docs == nil {
		return nil, nil
	}
	doc, ok := s.docs[docID]
	if !ok {
		return nil, nil
	}
	cp := doc
	return &cp, nil
}

func (s *stubDocDB) ListDocuments(_ context.Context, _, _, _ string, _ databases.Query, principal databases.Principal) (*databases.DocumentList, error) {
	s.lastListPrincipal = principal
	if s.list != nil {
		return s.list, nil
	}
	return &databases.DocumentList{}, nil
}

func (s *stubDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	if s.coll != nil {
		return s.coll, nil
	}
	return &databases.Collection{ID: "notes"}, nil
}

func (s *stubDocDB) EnsureCatalog(context.Context, string) error { return nil }
func (s *stubDocDB) UpsertDocument(context.Context, string, string, string, databases.Document, []string, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (s *stubDocDB) DeleteDocument(context.Context, string, string, string, string, databases.DeleteOptions, databases.Principal) error {
	return nil
}
func (s *stubDocDB) CountDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (int64, error) {
	return 0, nil
}
func (s *stubDocDB) CreateDatabase(context.Context, string, string, string) error { return nil }
func (s *stubDocDB) GetDatabase(context.Context, string, string) (*databases.Database, error) {
	return nil, nil
}
func (s *stubDocDB) ListDatabases(context.Context, string) ([]databases.Database, error) {
	return nil, nil
}
func (s *stubDocDB) DeleteDatabase(context.Context, string, string) error { return nil }
func (s *stubDocDB) CreateCollection(context.Context, string, string, string, string, []databases.Attribute, []databases.Index, []databases.Permission, bool) error {
	return nil
}
func (s *stubDocDB) ListCollections(context.Context, string, string, databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (s *stubDocDB) DeleteCollection(context.Context, string, string, string) error { return nil }
func (s *stubDocDB) UpdateCollection(context.Context, string, string, string, databases.CollectionPatch) error {
	return nil
}
func (s *stubDocDB) CreateAttribute(context.Context, string, string, string, databases.Attribute) error {
	return nil
}
func (s *stubDocDB) DeleteAttribute(context.Context, string, string, string, string) error {
	return nil
}
func (s *stubDocDB) CreateIndex(context.Context, string, string, string, databases.Index) error {
	return nil
}
func (s *stubDocDB) DeleteIndex(context.Context, string, string, string, string) error { return nil }
func (s *stubDocDB) AggregateDocuments(context.Context, string, string, string, databases.Query, []databases.AggregateSpec, string, databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (s *stubDocDB) BulkUpdateDocuments(context.Context, string, string, string, []string, map[string]any, []databases.Permission, databases.Principal) (int64, error) {
	return 0, nil
}
func (s *stubDocDB) BulkDeleteDocuments(context.Context, string, string, string, []string, databases.Principal) (int64, error) {
	return 0, nil
}

func (*stubDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (*stubDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	panic("ListChanges: not implemented in test fake")
}

var _ databases.DocumentDB = (*stubDocDB)(nil)

func (s stubProjects) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }
