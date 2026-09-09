package server

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeProjectRepo 是最小 projects.Repository 桩。
type fakeProjectRepo struct{}

func (fakeProjectRepo) CreateProject(context.Context, *projects.Project) error { return nil }
func (fakeProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	return &projects.Project{ID: id}, nil
}
func (fakeProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (fakeProjectRepo) ListProjects(context.Context) ([]projects.Project, error) { return nil, nil }
func (fakeProjectRepo) UpdateProject(context.Context, *projects.Project) error   { return nil }
func (fakeProjectRepo) DeleteProject(context.Context, string) error              { return nil }

// fakeDocDB 是内存版 DocumentDB，支持 GetDocument/ListDocuments（equal 过滤 +
// offset 分页）/UpdateDocument/CreateDocument/DeleteDocument，
// 其余方法不参与测试路径。
type fakeDocDB struct {
	docs map[string]map[string]databases.Document
}

func newFakeDocDB() *fakeDocDB {
	return &fakeDocDB{docs: map[string]map[string]databases.Document{}}
}

//nolint:unused
func (f *fakeDocDB) seed(collectionID string, docs ...databases.Document) {
	if f.docs[collectionID] == nil {
		f.docs[collectionID] = map[string]databases.Document{}
	}
	for _, d := range docs {
		f.docs[collectionID][d.ID] = d
	}
}

func (f *fakeDocDB) GetDocument(_ context.Context, _, _, collectionID, docID string, _ databases.Principal) (*databases.Document, error) {
	doc, ok := f.docs[collectionID][docID]
	if !ok {
		return nil, nil
	}
	return &doc, nil
}

func (f *fakeDocDB) ListDocuments(_ context.Context, _, _, collectionID string, q databases.Query, _ databases.Principal) (*databases.DocumentList, error) {
	parsed := q.AST
	if parsed == nil {
		var err error
		parsed, err = query.ParseMany(q.Queries)
		if err != nil {
			return nil, err
		}
	}
	var matched []databases.Document
	for _, doc := range f.docs[collectionID] {
		if fakeMatches(parsed, doc) {
			matched = append(matched, doc)
		}
	}
	total := int64(len(matched))
	offset := 0
	if q.PageToken != "" {
		off, err := crud.DecodePageToken(q.PageToken)
		if err != nil {
			return nil, err
		}
		offset = off
	}
	limit := parsed.Limit
	if limit == 0 {
		limit = int(q.PageSize)
	}
	if limit <= 0 {
		limit = 50
	}
	if offset > len(matched) {
		offset = len(matched)
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	next := ""
	if end < len(matched) {
		token, err := crud.EncodePageToken(end)
		if err != nil {
			return nil, err
		}
		next = token
	}
	return &databases.DocumentList{
		Documents:     append([]databases.Document{}, matched[offset:end]...),
		TotalCount:    total,
		NextPageToken: next,
	}, nil
}

func fakeMatches(parsed *query.Query, doc databases.Document) bool {
	ok := true
	parsed.WalkLeaves(func(fl query.Filter) {
		if !ok {
			return
		}
		switch fl.Op {
		case query.OpEqual:
			v, _ := doc.Data[fl.Attribute].(string)
			if len(fl.Values) == 0 || v != fl.Values[0] {
				ok = false
			}
		case query.OpNotEqual:
			v, _ := doc.Data[fl.Attribute].(string)
			if len(fl.Values) > 0 && v == fl.Values[0] {
				ok = false
			}
		}
	})
	return ok
}

func (f *fakeDocDB) UpdateDocument(_ context.Context, _, _, collectionID string, update databases.DocumentUpdate, _ databases.Principal) (databases.Document, error) {
	doc, ok := f.docs[collectionID][update.Document.ID]
	if !ok {
		return databases.Document{}, errors.New("document not found")
	}
	for k, v := range update.Document.Data {
		doc.Data[k] = v
	}
	// 模拟 OCC：版本 +1（单元测试 use-case 需要非零 version）。
	doc.Version++
	f.docs[collectionID][doc.ID] = doc
	return doc, nil
}

func (f *fakeDocDB) CreateDocument(_ context.Context, _, _, collectionID string, doc databases.Document, _ []databases.Permission, _ databases.Principal) (databases.Document, error) {
	if f.docs[collectionID] == nil {
		f.docs[collectionID] = map[string]databases.Document{}
	}
	// 模拟用户集合 OCC：创建后版本为 1。
	doc.Version = 1
	f.docs[collectionID][doc.ID] = doc
	return doc, nil
}

func (f *fakeDocDB) DeleteDocument(_ context.Context, _, _, collectionID, docID string, _ databases.DeleteOptions, _ databases.Principal) error {
	delete(f.docs[collectionID], docID)
	return nil
}

func (f *fakeDocDB) EnsureCatalog(context.Context, string) error { return nil }

func (f *fakeDocDB) CreateDatabase(context.Context, string, string, string) error { return nil }
func (f *fakeDocDB) GetDatabase(context.Context, string, string) (*databases.Database, error) {
	return nil, nil
}
func (f *fakeDocDB) ListDatabases(context.Context, string) ([]databases.Database, error) {
	return nil, nil
}
func (f *fakeDocDB) DeleteDatabase(context.Context, string, string) error { return nil }
func (f *fakeDocDB) CreateCollection(context.Context, string, string, string, string, []databases.Attribute, []databases.Index, []databases.Permission, bool) error {
	return nil
}
func (f *fakeDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return nil, nil
}
func (f *fakeDocDB) ListCollections(context.Context, string, string, databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (f *fakeDocDB) DeleteCollection(context.Context, string, string, string) error { return nil }
func (f *fakeDocDB) UpdateCollection(context.Context, string, string, string, databases.CollectionPatch) error {
	return nil
}
func (f *fakeDocDB) CreateAttribute(context.Context, string, string, string, databases.Attribute) error {
	return nil
}
func (f *fakeDocDB) DeleteAttribute(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeDocDB) CreateIndex(context.Context, string, string, string, databases.Index) error {
	return nil
}
func (f *fakeDocDB) DeleteIndex(context.Context, string, string, string, string) error { return nil }
func (f *fakeDocDB) UpsertDocument(context.Context, string, string, string, databases.Document, []string, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (f *fakeDocDB) CountDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (int64, error) {
	return 0, nil
}
func (f *fakeDocDB) AggregateDocuments(context.Context, string, string, string, databases.Query, []databases.AggregateSpec, string, databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (f *fakeDocDB) BulkUpdateDocuments(context.Context, string, string, string, []string, map[string]any, []databases.Permission, databases.Principal) (int64, error) {
	return 0, nil
}
func (f *fakeDocDB) BulkDeleteDocuments(context.Context, string, string, string, []string, databases.Principal) (int64, error) {
	return 0, nil
}

func (*fakeDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (*fakeDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	panic("ListChanges: not implemented in test fake")
}

var _ databases.DocumentDB = (*fakeDocDB)(nil)

func fakeMem(id, groupID, userID, statusVal string, roles []string) *groups.Membership {
	return &groups.Membership{ID: id, GroupID: groupID, UserID: userID, Status: statusVal, Roles: roles}
}

func testGroupsUC(rows ...*groups.Membership) (*Groups, *memMembershipRepo, *memGroupRepo) {
	g := newMemGroupRepo()
	g.seed(&groups.Group{ID: "group-1", Total: int64(len(rows))})
	m := newMemMembershipRepo()
	m.groups = g
	for _, row := range rows {
		m.seed(row)
	}
	uc := NewGroups(fakeProjectRepo{}, newMemUserRepo(), g, m)
	return uc, m, g
}

// TestGroups_LastOwnerProtection_DeleteMembership: 删除唯一 accepted owner → 拒绝；
// 删除非 owner 或存在第二 owner 时放行。
func TestGroups_LastOwnerProtection_DeleteMembership(t *testing.T) {
	// 写方法入口双面守卫：server 面（admin 会话）主体注入。
	adminCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{ActorKind: shared.ActorKindAdmin})
	t.Run("last owner rejected", func(t *testing.T) {
		uc, _, _ := testGroupsUC(
			fakeMem("m-owner", "group-1", "user-a", groups.StatusAccepted, []string{groups.RoleOwner}),
			fakeMem("m-member", "group-1", "user-b", groups.StatusAccepted, []string{groups.RoleMember}),
		)
		err := uc.DeleteMembership(adminCtx, "proj-1", "group-1", "m-owner", databases.Principal{Roles: []string{"admin"}})
		require.Error(t, err)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("second owner allows removal", func(t *testing.T) {
		uc, mems, _ := testGroupsUC(
			fakeMem("m-owner1", "group-1", "user-a", groups.StatusAccepted, []string{groups.RoleOwner}),
			fakeMem("m-owner2", "group-1", "user-b", groups.StatusAccepted, []string{groups.RoleOwner}),
		)
		err := uc.DeleteMembership(adminCtx, "proj-1", "group-1", "m-owner1", databases.Principal{Roles: []string{"admin"}})
		require.NoError(t, err)
		got, _ := mems.GetByID(context.Background(), "proj-1", "m-owner1")
		require.Nil(t, got)
	})

	t.Run("pending owner removal allowed", func(t *testing.T) {
		uc, _, _ := testGroupsUC(
			fakeMem("m-pending", "group-1", "user-a", groups.StatusPending, []string{groups.RoleOwner}),
		)
		err := uc.DeleteMembership(adminCtx, "proj-1", "group-1", "m-pending", databases.Principal{Roles: []string{"admin"}})
		require.NoError(t, err)
	})

	t.Run("non-owner removal allowed", func(t *testing.T) {
		uc, _, _ := testGroupsUC(
			fakeMem("m-owner", "group-1", "user-a", groups.StatusAccepted, []string{groups.RoleOwner}),
			fakeMem("m-member", "group-1", "user-b", groups.StatusAccepted, []string{groups.RoleMember}),
		)
		err := uc.DeleteMembership(adminCtx, "proj-1", "group-1", "m-member", databases.Principal{Roles: []string{"admin"}})
		require.NoError(t, err)
	})
}

// TestGroups_LastOwnerProtection_UpdateMembership: 唯一 owner 降级 → 拒绝；
// 有第二 owner 时降级放行；仍保留 owner 角色时放行。
func TestGroups_LastOwnerProtection_UpdateMembership(t *testing.T) {
	// 写方法入口双面守卫：server 面（admin 会话）主体注入。
	adminCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{ActorKind: shared.ActorKindAdmin})
	t.Run("downgrade last owner rejected", func(t *testing.T) {
		uc, _, _ := testGroupsUC(
			fakeMem("m-owner", "group-1", "user-a", groups.StatusAccepted, []string{groups.RoleOwner}),
			fakeMem("m-member", "group-1", "user-b", groups.StatusAccepted, []string{groups.RoleMember}),
		)
		_, err := uc.UpdateMembership(adminCtx, "proj-1", "group-1", "m-owner",
			UpdateMembershipCommand{Roles: []string{groups.RoleMember}}, databases.Principal{Roles: []string{"admin"}})
		require.Error(t, err)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("downgrade with second owner allowed", func(t *testing.T) {
		uc, _, _ := testGroupsUC(
			fakeMem("m-owner1", "group-1", "user-a", groups.StatusAccepted, []string{groups.RoleOwner}),
			fakeMem("m-owner2", "group-1", "user-b", groups.StatusAccepted, []string{groups.RoleOwner}),
		)
		updated, err := uc.UpdateMembership(adminCtx, "proj-1", "group-1", "m-owner1",
			UpdateMembershipCommand{Roles: []string{groups.RoleMember}}, databases.Principal{Roles: []string{"admin"}})
		require.NoError(t, err)
		require.Equal(t, []string{groups.RoleMember}, updated.Data["roles"])
	})

	t.Run("keep owner role allowed", func(t *testing.T) {
		uc, _, _ := testGroupsUC(
			fakeMem("m-owner", "group-1", "user-a", groups.StatusAccepted, []string{groups.RoleOwner}),
		)
		updated, err := uc.UpdateMembership(adminCtx, "proj-1", "group-1", "m-owner",
			UpdateMembershipCommand{Roles: []string{groups.RoleOwner, groups.RoleAdmin}}, databases.Principal{Roles: []string{"admin"}})
		require.NoError(t, err)
		require.Equal(t, []string{groups.RoleOwner, groups.RoleAdmin}, updated.Data["roles"])
	})
}

// TestUsers_UpdateUserEmailUniqueness: 改邮箱撞他人邮箱 → AlreadyExists；
// 改回自身邮箱或新唯一邮箱 → 成功。
func TestUsers_UpdateUserEmailUniqueness(t *testing.T) {
	usersMem := newMemUserRepo()
	usersMem.seed(&domainusers.User{ID: "user-a", Email: "a@torchwood.local"})
	usersMem.seed(&domainusers.User{ID: "user-b", Email: "b@torchwood.local"})
	// M5 C3：email/status 变更即撤会话，测试注入内存 session service
	//（sessionRepo 桩同用一实例，DeleteByUser 幂等无副作用）。
	sessions := auth.NewSessionService(&config.AppConfig{}, newMemSessionRepo(), nil, nil)
	uc := NewUsers(fakeProjectRepo{}, sessions, &clients.Database{}, usersMem, newMemSessionRepo(), newMemGroupRepo(), newMemMembershipRepo())
	// Round3 H1-3：UpdateUser 现在要求 Server 写主体（admin 会话 / API key）。
	actorCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"},
		Permissions: []string{"users.write"},
	})

	t.Run("duplicate email rejected", func(t *testing.T) {
		_, err := uc.UpdateUser(actorCtx, "proj-1", "user-a", map[string]any{"email": "B@Torchwood.Local"},
			databases.Principal{Roles: []string{"keys"}})
		require.Error(t, err)
		require.Equal(t, codes.AlreadyExists, status.Code(err))
	})

	t.Run("own email unchanged allowed", func(t *testing.T) {
		_, err := uc.UpdateUser(actorCtx, "proj-1", "user-a", map[string]any{"email": "A@torchwood.local"},
			databases.Principal{Roles: []string{"keys"}})
		require.NoError(t, err)
	})

	t.Run("new unique email allowed", func(t *testing.T) {
		updated, err := uc.UpdateUser(actorCtx, "proj-1", "user-a", map[string]any{"email": "c@torchwood.local"},
			databases.Principal{Roles: []string{"keys"}})
		require.NoError(t, err)
		require.Equal(t, "c@torchwood.local", updated.Data["email"])
		require.Equal(t, false, updated.Data["email_verified"])
	})
}

func (r fakeProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }
