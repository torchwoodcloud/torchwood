package documents

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCreateDocument_DataRequired(t *testing.T) {
	core := New(newMemDocDB(), nil)
	_, _, err := core.CreateDocument(context.Background(), "p", "app", "notes", "d1", nil, nil, databases.Principal{}, "", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestValidateDocumentPayload（redesign §11-J H1）：载荷总量 ≤1 MiB、单属性值
// ≤256 KiB（与事件信封截断阈值对齐），超限 InvalidArgument 且消息带
// DOCUMENT.TOO_LARGE 域码；四个写路径共用同一校验。
func TestValidateDocumentPayload(t *testing.T) {
	big := strings.Repeat("a", 256*1024+1)
	err := ValidateDocumentPayload(map[string]any{"blob": big})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "DOCUMENT.TOO_LARGE")

	mid := strings.Repeat("a", 200*1024)
	err = ValidateDocumentPayload(map[string]any{
		"a": mid, "b": mid, "c": mid, "d": mid, "e": mid, "f": mid,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "DOCUMENT.TOO_LARGE")

	require.NoError(t, ValidateDocumentPayload(map[string]any{"ok": "small"}))
	require.NoError(t, ValidateDocumentPayload(nil))
	require.NoError(t, ValidateDocumentPayload(map[string]any{
		"a": strings.Repeat("a", 256*1024-2), // 编码后恰好 256 KiB（含 JSON 引号）：合法
	}))

	// 写路径接入：Create 与 Upsert 超限在进 adapter 前被拒。
	rec := newMemDocDB()
	core := New(rec, nil)
	_, _, err = core.CreateDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"blob": big}, nil, databases.Principal{}, "", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, rec.creates)
	_, _, err = core.UpsertDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"blob": big}, []string{"x"}, nil, databases.Principal{}, "", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, rec.upserts)
}

func TestCreateDocument_GrantRequiresHeldRole(t *testing.T) {
	rec := newMemDocDB()
	core := New(rec, nil)
	user := databases.Principal{Roles: []string{"users", "user:u1"}}
	_, _, err := core.CreateDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"t": 1}, []databases.Permission{
		{Type: "update", Role: "user:other"},
	}, user, "", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, rec.creates)

	_, _, err = core.CreateDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"t": 1}, []databases.Permission{
		{Type: "update", Role: "user:other"},
	}, user, "", WriteOptions{AllowPrivilegedGrant: true})
	require.NoError(t, err)
	require.Equal(t, 1, rec.creates)
}

func TestUpdateDocument_VersionRequired(t *testing.T) {
	core := New(newMemDocDB(), nil)
	_, _, err := core.UpdateDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, nil, nil, databases.Principal{}, nil, "", WriteOptions{})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	// Phase 1 裁决②：显式 0 与缺省不同码——InvalidArgument / version_invalid。
	zero := int64(0)
	_, _, err = core.UpdateDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, nil, nil, databases.Principal{}, &zero, "", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "version_invalid")
}

func TestGetDocument_NotFound(t *testing.T) {
	core := New(newMemDocDB(), nil)
	_, err := core.GetDocument(context.Background(), "p", "app", "notes", "missing", databases.Principal{})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestListDocuments_KeepsDocumentVersion(t *testing.T) {
	rec := newMemDocDB()
	rec.docs["users/u1"] = databases.Document{ID: "u1", Version: 7, Data: map[string]any{"email": "a@b.c"}}
	core := New(rec, nil)
	result, err := core.ListDocuments(context.Background(), "p", "app", "users", databases.Query{}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(1), result.TotalCount)
	require.Equal(t, int64(7), result.Documents[0].Version)
}

func TestUpsertDocument_ConflictColumnsRequired(t *testing.T) {
	core := New(newMemDocDB(), nil)
	_, _, err := core.UpsertDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, nil, databases.Principal{}, "", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestBulkUpdateDocuments_MaxOperations(t *testing.T) {
	rec := newMemDocDB()
	core := New(rec, nil)
	ids := make([]string, MaxBulkOperations+1)
	for i := range ids {
		ids[i] = "x"
	}
	_, _, err := core.BulkUpdateDocuments(context.Background(), "p", "app", "notes", ids, map[string]any{"t": 1}, nil, databases.Principal{}, "", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, rec.bulkUpdates)
}

type memDocDB struct {
	docs        map[string]databases.Document
	creates     int
	upserts     int
	bulkUpdates int
}

func newMemDocDB() *memDocDB {
	return &memDocDB{docs: map[string]databases.Document{}}
}

func (m *memDocDB) key(collectionID, docID string) string { return collectionID + "/" + docID }

func (m *memDocDB) CreateDocument(_ context.Context, _, _, collectionID string, doc databases.Document, _ []databases.Permission, _ databases.Principal) (databases.Document, error) {
	m.creates++
	if doc.ID == "" {
		doc.ID = "generated"
	}
	doc.Version = 1
	m.docs[m.key(collectionID, doc.ID)] = doc
	return doc, nil
}

func (m *memDocDB) GetDocument(_ context.Context, _, _, collectionID, docID string, _ databases.Principal) (*databases.Document, error) {
	doc, ok := m.docs[m.key(collectionID, docID)]
	if !ok {
		return nil, nil
	}
	cp := doc
	return &cp, nil
}

func (m *memDocDB) ListDocuments(_ context.Context, _, _, collectionID string, _ databases.Query, _ databases.Principal) (*databases.DocumentList, error) {
	prefix := collectionID + "/"
	var out []databases.Document
	for k, doc := range m.docs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, doc)
		}
	}
	return &databases.DocumentList{Documents: out, TotalCount: int64(len(out))}, nil
}

func (m *memDocDB) UpdateDocument(_ context.Context, _, _, collectionID string, update databases.DocumentUpdate, _ databases.Principal) (databases.Document, error) {
	doc, ok := m.docs[m.key(collectionID, update.Document.ID)]
	if !ok {
		return databases.Document{}, databases.ErrDocumentNotFound
	}
	for k, v := range update.Document.Data {
		if doc.Data == nil {
			doc.Data = map[string]any{}
		}
		doc.Data[k] = v
	}
	doc.Version++
	m.docs[m.key(collectionID, doc.ID)] = doc
	return doc, nil
}

func (m *memDocDB) UpsertDocument(_ context.Context, _, _, collectionID string, doc databases.Document, _ []string, _ []databases.Permission, _ databases.Principal) (databases.Document, error) {
	m.upserts++
	if existing, ok := m.docs[m.key(collectionID, doc.ID)]; ok {
		existing.Data = doc.Data
		existing.Version++
		m.docs[m.key(collectionID, doc.ID)] = existing
		return existing, nil
	}
	doc.Version = 1
	m.docs[m.key(collectionID, doc.ID)] = doc
	return doc, nil
}

func (m *memDocDB) DeleteDocument(_ context.Context, _, _, collectionID, docID string, _ databases.DeleteOptions, _ databases.Principal) error {
	delete(m.docs, m.key(collectionID, docID))
	return nil
}

func (m *memDocDB) CountDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (int64, error) {
	return int64(len(m.docs)), nil
}

func (m *memDocDB) BulkUpdateDocuments(context.Context, string, string, string, []string, map[string]any, []databases.Permission, databases.Principal) (int64, error) {
	m.bulkUpdates++
	return 0, nil
}

func (m *memDocDB) BulkDeleteDocuments(context.Context, string, string, string, []string, databases.Principal) (int64, error) {
	return 0, nil
}

func (m *memDocDB) CreateDatabase(context.Context, string, string, string) error { return nil }
func (m *memDocDB) GetDatabase(context.Context, string, string) (*databases.Database, error) {
	return nil, nil
}
func (m *memDocDB) ListDatabases(context.Context, string) ([]databases.Database, error) {
	return nil, nil
}
func (m *memDocDB) DeleteDatabase(context.Context, string, string) error { return nil }
func (m *memDocDB) CreateCollection(context.Context, string, string, string, string, []databases.Attribute, []databases.Index, []databases.Permission, bool) error {
	return nil
}
func (m *memDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return &databases.Collection{ID: "notes"}, nil
}
func (m *memDocDB) ListCollections(context.Context, string, string, databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (m *memDocDB) DeleteCollection(context.Context, string, string, string) error { return nil }
func (m *memDocDB) UpdateCollection(context.Context, string, string, string, databases.CollectionPatch) error {
	return nil
}
func (m *memDocDB) CreateAttribute(context.Context, string, string, string, databases.Attribute) error {
	return nil
}
func (m *memDocDB) DeleteAttribute(context.Context, string, string, string, string) error {
	return nil
}
func (m *memDocDB) CreateIndex(context.Context, string, string, string, databases.Index) error {
	return nil
}
func (m *memDocDB) DeleteIndex(context.Context, string, string, string, string) error { return nil }
func (m *memDocDB) AggregateDocuments(context.Context, string, string, string, databases.Query, []databases.AggregateSpec, string, databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (m *memDocDB) EnsureCatalog(context.Context, string) error { return nil }

func (m *memDocDB) ExecuteTransactions(_ context.Context, _, _ string, ops []databases.TransactionOp, _ databases.TransactionMode, _ databases.Principal) ([]databases.TransactionOpResult, error) {
	out := make([]databases.TransactionOpResult, 0, len(ops))
	for i, op := range ops {
		switch op.Type {
		case databases.TransactionOpCreate, databases.TransactionOpUpsert:
			doc := databases.Document{ID: op.DocumentID, Data: op.Data, Version: 1}
			m.docs[m.key(op.CollectionID, doc.ID)] = doc
			out = append(out, databases.TransactionOpResult{Index: i, OK: true, DocumentID: doc.ID, Version: doc.Version})
		default:
			out = append(out, databases.TransactionOpResult{Index: i, OK: true, DocumentID: op.DocumentID})
		}
	}
	return out, nil
}

var _ databases.DocumentDB = (*memDocDB)(nil)

func (*memDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	return nil, false, 0, nil
}
