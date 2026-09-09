package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 写幂等集成（redesign §4.1/§10.1）：bunrepo IdempotencyStore 真库路径——
// 重放、KEY_CONFLICT、TTL 过期重执行。
func newIdempotencyTestSetup(t *testing.T) (context.Context, *Databases, *databases.Principal, string, func()) {
	t.Helper()
	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	cleanupDB := func() { _ = db.Close() }
	projectID, _, cleanupProject := testutil.CreateTestProject(ctx, db)

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	store := bunrepo.NewIdempotencyStore(db)
	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, store)
	principal := databases.Principal{Roles: []string{"keys"}, KeyID: "key_1"}

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	return ctx, uc, &principal, projectID, func() {
		cleanupProject()
		cleanupDB()
	}
}

func TestDatabases_IdempotencyReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, principal, projectID, cleanup := newIdempotencyTestSetup(t)
	defer cleanup()

	first, replayed, err := uc.CreateDocument(ctx, projectID, "app", "notes", "doc-1", map[string]any{"title": "a"}, databases.DefaultCollectionPermissions(), *principal, "req-100")
	require.NoError(t, err)
	require.False(t, replayed)

	second, replayed, err := uc.CreateDocument(ctx, projectID, "app", "notes", "doc-1", map[string]any{"title": "a"}, databases.DefaultCollectionPermissions(), *principal, "req-100")
	require.NoError(t, err)
	require.True(t, replayed)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.Version, second.Version)
	require.True(t, first.CreatedAt.Equal(second.CreatedAt), "重放返回首次响应的创建时间")

	count, err := uc.CountDocuments(ctx, projectID, "app", "notes", databases.Query{}, *principal)
	require.NoError(t, err)
	require.Equal(t, int64(1), count, "重放不得产生第二行")

	// 同 key 异体 → KEY_CONFLICT。
	_, _, err = uc.CreateDocument(ctx, projectID, "app", "notes", "doc-2", map[string]any{"title": "b"}, databases.DefaultCollectionPermissions(), *principal, "req-100")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), databases.ErrCodeIdempotencyKeyConflict)
}

func TestDatabases_IdempotencyTTLExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, principal, projectID, cleanup := newIdempotencyTestSetup(t)
	defer cleanup()

	_, replayed, err := uc.CreateDocument(ctx, projectID, "app", "notes", "doc-1", map[string]any{"title": "a"}, databases.DefaultCollectionPermissions(), *principal, "req-200")
	require.NoError(t, err)
	require.False(t, replayed)

	// 直接把行置为过期（TTL 24h 不等真实时钟），同 key 同请求重新执行。
	key := databases.IdempotencyKey{ProjectID: projectID, ActorID: "key:key_1", RequestID: "req-200"}
	require.NoError(t, idempotencyExpireAt(ctx, uc, key, time.Now().Add(-time.Minute)))

	_, replayed, err = uc.CreateDocument(ctx, projectID, "app", "notes", "doc-1", map[string]any{"title": "a"}, databases.DefaultCollectionPermissions(), *principal, "req-200")
	require.False(t, replayed, "TTL 过期后同 key 是新请求（重放能力随行过期）")
	require.Equal(t, codes.AlreadyExists, status.Code(err), "重执行命中首次写入的既有行（重复 create → ALREADY_EXISTS）")
}

// idempotencyExpireAt 通过 store 的测试辅助把行置为过期。
func idempotencyExpireAt(ctx context.Context, uc *Databases, key databases.IdempotencyKey, at time.Time) error {
	if s, ok := uc.idem.(*bunrepo.IdempotencyStore); ok {
		return s.ExpireAt(ctx, key, at)
	}
	return nil
}
