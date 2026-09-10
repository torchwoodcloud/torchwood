package documents

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// memIdemStore 是 IdempotencyStore 的内存实现，语义对齐 bunrepo 版：
// TryClaim 原子仲裁、Done 携带 payload、指纹冲突返回 ErrIdempotencyKeyConflict。
type memIdemStore struct {
	mu       sync.Mutex
	rows     map[databases.IdempotencyKey]*memIdemRow
	tokenSeq int
}

type memIdemRow struct {
	fingerprint string
	token       string
	state       databases.IdempotencyClaimState
	payload     []byte
}

func newMemIdemStore() *memIdemStore {
	return &memIdemStore{rows: map[databases.IdempotencyKey]*memIdemRow{}}
}

func (m *memIdemStore) TryClaim(_ context.Context, key databases.IdempotencyKey, fingerprint string) (databases.IdempotencyClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[key]; ok {
		if row.fingerprint != fingerprint {
			return databases.IdempotencyClaim{}, databases.ErrIdempotencyKeyConflict
		}
		switch row.state {
		case databases.IdempotencyClaimDone:
			return databases.IdempotencyClaim{State: databases.IdempotencyClaimDone, Payload: row.payload}, nil
		default:
			return databases.IdempotencyClaim{State: databases.IdempotencyClaimInFlight}, nil
		}
	}
	m.tokenSeq++
	row := &memIdemRow{fingerprint: fingerprint, token: "t", state: databases.IdempotencyClaimAcquired}
	m.rows[key] = row
	return databases.IdempotencyClaim{State: databases.IdempotencyClaimAcquired, Token: row.token}, nil
}

func (m *memIdemStore) Complete(_ context.Context, key databases.IdempotencyKey, token string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[key]; ok && row.token == token && row.state == databases.IdempotencyClaimAcquired {
		row.state = databases.IdempotencyClaimDone
		row.payload = payload
	}
	return nil
}

func (m *memIdemStore) Release(_ context.Context, key databases.IdempotencyKey, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[key]; ok && row.token == token && row.state == databases.IdempotencyClaimAcquired {
		delete(m.rows, key)
	}
	return nil
}

// keyPrincipal 返回幂等键测试用的固定 principal。
func keyPrincipal() databases.Principal {
	return databases.Principal{Roles: []string{"users", "user:u1"}}
}

// TestCreateDocument_IdempotentReplay：同 key 重放返回原响应且 replayed=true，
// 底层只执行一次。
func TestCreateDocument_IdempotentReplay(t *testing.T) {
	rec := newMemDocDB()
	idem := newMemIdemStore()
	core := New(rec, idem)
	ctx := context.Background()

	first, replayed, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.NoError(t, err)
	require.False(t, replayed)
	require.Equal(t, 1, rec.creates)

	second, replayed, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.NoError(t, err)
	require.True(t, replayed)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.Version, second.Version)
	// JSON 往返会把 Data 内数值统一为 float64（与 proto structpb 的 number 语义
	// 一致）——按 proto 表示断言等价。
	firstPB, err := structpb.NewStruct(first.Data)
	require.NoError(t, err)
	secondPB, err := structpb.NewStruct(second.Data)
	require.NoError(t, err)
	require.Equal(t, firstPB.AsMap(), secondPB.AsMap())
	require.Equal(t, 1, rec.creates, "重放不得重复执行")
}

// TestCreateDocument_IdempotencyKeyConflict：同 key 携带不同请求 → KEY_CONFLICT。
func TestCreateDocument_IdempotencyKeyConflict(t *testing.T) {
	core := New(newMemDocDB(), newMemIdemStore())
	ctx := context.Background()

	_, _, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.NoError(t, err)

	_, _, err = core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 2}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), databases.ErrCodeIdempotencyKeyConflict)
}

// TestCreateDocument_IdempotencyInProgress：同 key in-flight 且轮询超时 →
// IN_PROGRESS（Aborted，retryable）。
func TestCreateDocument_IdempotencyInProgress(t *testing.T) {
	restore := idempotencyWaitBudget
	idempotencyWaitBudget = 150 * time.Millisecond
	t.Cleanup(func() { idempotencyWaitBudget = restore })

	idem := newMemIdemStore()
	core := New(newMemDocDB(), idem)
	// 与 CreateDocument("req-1") 的指纹一致：先执行一次拿到指纹，再让同键处于
	// in_flight（模拟并发请求已认领但未完成）。
	_, _, err := core.CreateDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.NoError(t, err)
	idem.mu.Lock()
	for _, row := range idem.rows {
		row.state = databases.IdempotencyClaimInFlight
	}
	idem.mu.Unlock()

	_, _, err = core.CreateDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.Equal(t, codes.Aborted, status.Code(err))
	require.Contains(t, err.Error(), databases.ErrCodeIdempotencyInProgress)
}

// TestCreateDocument_IdempotencyFailureNotCached：失败不缓存，重试重新执行。
func TestCreateDocument_IdempotencyFailureNotCached(t *testing.T) {
	rec := newMemDocDB()
	idem := newMemIdemStore()
	core := New(rec, idem)
	ctx := context.Background()

	// 未持有 user:other → 授予校验失败（InvalidArgument）。
	_, _, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, []databases.Permission{
		{Type: "update", Role: "user:other"},
	}, keyPrincipal(), "req-1", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, rec.creates)

	// 重试同一 key（修正后的请求）应重新执行而不是被失败结果拦截。
	_, replayed, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.NoError(t, err)
	require.False(t, replayed)
	require.Equal(t, 1, rec.creates)
}

// TestCreateDocument_IdempotencyActorScope：不同 actor 同 key 不冲突（键作用域）。
func TestCreateDocument_IdempotencyActorScope(t *testing.T) {
	rec := newMemDocDB()
	core := New(rec, newMemIdemStore())
	ctx := context.Background()
	other := databases.Principal{Roles: []string{"users", "user:u2"}}

	_, _, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.NoError(t, err)
	_, replayed, err := core.CreateDocument(ctx, "p", "app", "notes", "d2", map[string]any{"t": 1}, nil, other, "req-1", WriteOptions{})
	require.NoError(t, err)
	require.False(t, replayed, "不同 actor 同 key 是独立请求")
	require.Equal(t, 2, rec.creates)
}

// TestCreateDocument_IdempotencyDisabledWithoutRequestID：无 request_id 直接执行。
func TestCreateDocument_IdempotencyDisabledWithoutRequestID(t *testing.T) {
	rec := newMemDocDB()
	core := New(rec, newMemIdemStore())
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, replayed, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "", WriteOptions{})
		require.NoError(t, err)
		require.False(t, replayed)
	}
	require.Equal(t, 2, rec.creates)
}

// TestDeleteDocument_IdempotentReplay：Delete 的重放（空载荷）。
func TestDeleteDocument_IdempotentReplay(t *testing.T) {
	rec := newMemDocDB()
	core := New(rec, newMemIdemStore())
	ctx := context.Background()
	v := int64(1)
	rec.docs["notes/d1"] = databases.Document{ID: "d1", Version: 1}

	replayed, err := core.DeleteDocument(ctx, "p", "app", "notes", "d1", keyPrincipal(), &v, "req-1")
	require.NoError(t, err)
	require.False(t, replayed)
	require.NotContains(t, rec.docs, "notes/d1")

	replayed, err = core.DeleteDocument(ctx, "p", "app", "notes", "d1", keyPrincipal(), &v, "req-1")
	require.NoError(t, err)
	require.True(t, replayed)
}

// TestBulkUpdateDocuments_IdempotentReplay：Bulk 的重放（int64 载荷往返）。
func TestBulkUpdateDocuments_IdempotentReplay(t *testing.T) {
	rec := newMemDocDB()
	core := New(rec, newMemIdemStore())
	ctx := context.Background()

	n, replayed, err := core.BulkUpdateDocuments(ctx, "p", "app", "notes", []string{"d1", "d2"}, map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.NoError(t, err)
	require.False(t, replayed)
	require.Equal(t, int64(0), n)
	require.Equal(t, 1, rec.bulkUpdates)

	n2, replayed, err := core.BulkUpdateDocuments(ctx, "p", "app", "notes", []string{"d2", "d1"}, map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.NoError(t, err)
	require.True(t, replayed, "批 ID 顺序不同不构成 KEY_CONFLICT（集合语义）")
	require.Equal(t, n, n2)
	require.Equal(t, 1, rec.bulkUpdates, "重放不得重复执行")
}

// TestExecuteTransactions_IdempotentReplay：execute-tx 的重放（PARTIAL per-op
// 结果整体往返）。
func TestExecuteTransactions_IdempotentReplay(t *testing.T) {
	rec := newMemDocDB()
	core := New(rec, newMemIdemStore())
	ctx := context.Background()
	ops := []databases.TransactionOp{{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "d1", Data: map[string]any{"t": 1}}}

	first, replayed, err := core.ExecuteTransactions(ctx, "p", "app", ops, databases.TransactionModePartial, keyPrincipal(), "req-1")
	require.NoError(t, err)
	require.False(t, replayed)
	require.Len(t, first, 1)

	second, replayed, err := core.ExecuteTransactions(ctx, "p", "app", ops, databases.TransactionModePartial, keyPrincipal(), "req-1")
	require.NoError(t, err)
	require.True(t, replayed)
	require.Equal(t, first, second)
}

// TestIdempotencyStoreUnavailable：store 故障时写请求失败（不静默降级为
// at-least-once——客户端显式要求幂等）。
func TestIdempotencyStoreUnavailable(t *testing.T) {
	core := New(newMemDocDB(), errIdemStore{})
	_, _, err := core.CreateDocument(context.Background(), "p", "app", "notes", "d1", map[string]any{"t": 1}, nil, keyPrincipal(), "req-1", WriteOptions{})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

type errIdemStore struct{}

func (errIdemStore) TryClaim(context.Context, databases.IdempotencyKey, string) (databases.IdempotencyClaim, error) {
	return databases.IdempotencyClaim{}, errors.New("store down")
}
func (errIdemStore) Complete(context.Context, databases.IdempotencyKey, string, []byte) error {
	return nil
}
func (errIdemStore) Release(context.Context, databases.IdempotencyKey, string) error { return nil }

// TestIdempotencyDomainCodes：域码映射（InvalidArgument/Aborted + retryable 表）。
func TestIdempotencyDomainCodes(t *testing.T) {
	require.Equal(t, codes.InvalidArgument, status.Code(shared.DomainStatus(databases.ErrCodeIdempotencyKeyConflict)))
	require.Equal(t, codes.Aborted, status.Code(shared.DomainStatus(databases.ErrCodeIdempotencyInProgress)))
	require.True(t, databases.ErrorCodeRetryable(databases.ErrCodeIdempotencyInProgress))
	require.False(t, databases.ErrorCodeRetryable(databases.ErrCodeIdempotencyKeyConflict))
}
