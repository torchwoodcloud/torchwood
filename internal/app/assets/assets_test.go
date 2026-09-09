package assets

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type memStore struct {
	defs     map[string]*domainassets.Def
	byCode   map[string]string
	holdings map[string]*domainassets.Holding
	ledger   []*domainassets.LedgerEntry
	byIdem   map[string]int
	outbox   []domainevents.Envelope
}

func newMemStore() *memStore {
	return &memStore{
		defs:     map[string]*domainassets.Def{},
		byCode:   map[string]string{},
		holdings: map[string]*domainassets.Holding{},
		byIdem:   map[string]int{},
	}
}

func (s *memStore) Run(_ context.Context, fn func(context.Context) error) error {
	snap := s.snapshot()
	if err := fn(context.Background()); err != nil {
		s.restore(snap)
		return err
	}
	return nil
}

func (s *memStore) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return s.Run(ctx, fn)
}

func (s *memStore) snapshot() memStore {
	out := memStore{
		defs:     map[string]*domainassets.Def{},
		byCode:   map[string]string{},
		holdings: map[string]*domainassets.Holding{},
		ledger:   append([]*domainassets.LedgerEntry(nil), s.ledger...),
		byIdem:   map[string]int{},
		outbox:   append([]domainevents.Envelope(nil), s.outbox...),
	}
	for k, v := range s.defs {
		cp := *v
		out.defs[k] = &cp
	}
	for k, v := range s.byCode {
		out.byCode[k] = v
	}
	for k, v := range s.holdings {
		out.holdings[k] = cloneHolding(v)
	}
	for k, v := range s.byIdem {
		out.byIdem[k] = v
	}
	for i, e := range out.ledger {
		cp := *e
		out.ledger[i] = &cp
	}
	return out
}

func (s *memStore) restore(snap memStore) {
	s.defs = snap.defs
	s.byCode = snap.byCode
	s.holdings = snap.holdings
	s.ledger = snap.ledger
	s.byIdem = snap.byIdem
	s.outbox = snap.outbox
}

func cloneHolding(h *domainassets.Holding) *domainassets.Holding {
	if h == nil {
		return nil
	}
	cp := *h
	if h.ExpiresAt != nil {
		t := *h.ExpiresAt
		cp.ExpiresAt = &t
	}
	if h.Metadata != nil {
		cp.Metadata = append(json.RawMessage(nil), h.Metadata...)
	}
	return &cp
}

func cloneEntry(e *domainassets.LedgerEntry) *domainassets.LedgerEntry {
	if e == nil {
		return nil
	}
	cp := *e
	if e.ExpiresAt != nil {
		t := *e.ExpiresAt
		cp.ExpiresAt = &t
	}
	return &cp
}

func (s *memStore) Insert(_ context.Context, def *domainassets.Def) error {
	k := def.ProjectID + "\x00" + def.Code
	if _, ok := s.byCode[k]; ok {
		return domainassets.ErrDuplicateCode
	}
	cp := *def
	s.defs[def.ID] = &cp
	s.byCode[k] = def.ID
	return nil
}

func (s *memStore) getDef(projectID, predID, predCode string) (*domainassets.Def, error) {
	var d *domainassets.Def
	if predID != "" {
		d = s.defs[predID]
	} else {
		id := s.byCode[projectID+"\x00"+predCode]
		d = s.defs[id]
	}
	if d == nil || d.ProjectID != projectID {
		return nil, nil
	}
	cp := *d
	return &cp, nil
}

func (s *memStore) GetByID(_ context.Context, projectID, defID string) (*domainassets.Def, error) {
	return s.getDef(projectID, defID, "")
}
func (s *memStore) GetByCode(_ context.Context, projectID, code string) (*domainassets.Def, error) {
	return s.getDef(projectID, "", code)
}
func (s *memStore) GetByCodeForShare(ctx context.Context, projectID, code string) (*domainassets.Def, error) {
	return s.GetByCode(ctx, projectID, code)
}
func (s *memStore) GetByIDForShare(ctx context.Context, projectID, defID string) (*domainassets.Def, error) {
	return s.GetByID(ctx, projectID, defID)
}
func (s *memStore) List(_ context.Context, projectID string, includeArchived bool, limit int, before time.Time) ([]domainassets.Def, error) {
	var out []domainassets.Def
	for _, d := range s.defs {
		if d.ProjectID != projectID || !d.CreatedAt.Before(before) {
			continue
		}
		if !includeArchived && d.Status != domainassets.DefStatusActive {
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (s *memStore) Update(_ context.Context, def *domainassets.Def) error {
	if s.defs[def.ID] == nil {
		return domainassets.ErrDefNotFound
	}
	cp := *def
	s.defs[def.ID] = &cp
	return nil
}

func (s *memStore) InsertHolding(_ context.Context, h *domainassets.Holding) error {
	s.holdings[h.ID] = cloneHolding(h)
	return nil
}

func (s *memStore) getHolding(projectID, id string) (*domainassets.Holding, error) {
	h := s.holdings[id]
	if h == nil || h.ProjectID != projectID {
		return nil, nil
	}
	return cloneHolding(h), nil
}

func (s *memStore) GetHoldingByID(ctx context.Context, projectID, id string) (*domainassets.Holding, error) {
	return s.getHolding(projectID, id)
}
func (s *memStore) GetHoldingByIDForUpdate(ctx context.Context, projectID, id string) (*domainassets.Holding, error) {
	return s.getHolding(projectID, id)
}

func (s *memStore) ListForUpdate(_ context.Context, projectID string, ownerType domainassets.OwnerType, ownerID, defID string) ([]domainassets.Holding, error) {
	var out []domainassets.Holding
	for _, h := range s.holdings {
		if h.ProjectID == projectID && h.OwnerType == ownerType && h.OwnerID == ownerID && h.DefID == defID {
			out = append(out, *cloneHolding(h))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpiresAt == nil && out[j].ExpiresAt == nil {
			return out[i].ID < out[j].ID
		}
		if out[i].ExpiresAt == nil {
			return false
		}
		if out[j].ExpiresAt == nil {
			return true
		}
		if !out[i].ExpiresAt.Equal(*out[j].ExpiresAt) {
			return out[i].ExpiresAt.Before(*out[j].ExpiresAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *memStore) ListByOwner(_ context.Context, projectID string, ownerType domainassets.OwnerType, ownerID string, limit int, before time.Time) ([]domainassets.Holding, error) {
	var out []domainassets.Holding
	for _, h := range s.holdings {
		if h.ProjectID == projectID && h.OwnerType == ownerType && h.OwnerID == ownerID && h.CreatedAt.Before(before) {
			out = append(out, *cloneHolding(h))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) UpdateHolding(_ context.Context, h *domainassets.Holding, expectVersion int64) error {
	cur := s.holdings[h.ID]
	if cur == nil || cur.Version != expectVersion {
		return domainassets.ErrConcurrent
	}
	s.holdings[h.ID] = cloneHolding(h)
	return nil
}

func (s *memStore) DeleteHolding(_ context.Context, projectID, holdingID string, expectVersion int64) error {
	cur := s.holdings[holdingID]
	if cur == nil || cur.ProjectID != projectID || cur.Version != expectVersion {
		return domainassets.ErrConcurrent
	}
	delete(s.holdings, holdingID)
	return nil
}

func (s *memStore) ListExpiredInProject(_ context.Context, projectID string, now time.Time, limit int) ([]domainassets.Holding, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []domainassets.Holding
	for _, h := range s.holdings {
		if h.ProjectID != projectID {
			continue
		}
		if h.ExpiresAt != nil && !h.ExpiresAt.After(now) {
			out = append(out, *cloneHolding(h))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(*out[j].ExpiresAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) ListAllHoldings(_ context.Context, projectID string) ([]domainassets.Holding, error) {
	var out []domainassets.Holding
	for _, h := range s.holdings {
		if h.ProjectID == projectID {
			out = append(out, *cloneHolding(h))
		}
	}
	return out, nil
}

func idemKey(projectID, key string) string { return projectID + "\x00" + key }

func (s *memStore) InsertIfAbsent(_ context.Context, e *domainassets.LedgerEntry) (*domainassets.LedgerEntry, bool, error) {
	k := idemKey(e.ProjectID, e.IdempotencyKey)
	if idx, ok := s.byIdem[k]; ok {
		return cloneEntry(s.ledger[idx]), false, nil
	}
	cp := cloneEntry(e)
	s.byIdem[k] = len(s.ledger)
	s.ledger = append(s.ledger, cp)
	return nil, true, nil
}

func (s *memStore) GetByIdempotencyKey(_ context.Context, projectID, key string) (*domainassets.LedgerEntry, error) {
	idx, ok := s.byIdem[idemKey(projectID, key)]
	if !ok {
		return nil, nil
	}
	return cloneEntry(s.ledger[idx]), nil
}

func (s *memStore) ListByRef(_ context.Context, projectID, refType, refID string) ([]domainassets.LedgerEntry, error) {
	var out []domainassets.LedgerEntry
	for _, e := range s.ledger {
		if e.ProjectID == projectID && e.RefType == refType && e.RefID == refID {
			out = append(out, *cloneEntry(e))
		}
	}
	return out, nil
}

func (s *memStore) ListLedgerByOwner(_ context.Context, projectID string, ownerType domainassets.OwnerType, ownerID, defID string, limit int, before time.Time) ([]domainassets.LedgerEntry, error) {
	var out []domainassets.LedgerEntry
	for _, e := range s.ledger {
		if e.ProjectID != projectID || e.OwnerType != ownerType || e.OwnerID != ownerID || !e.CreatedAt.Before(before) {
			continue
		}
		if defID != "" && e.DefID != defID {
			continue
		}
		out = append(out, *cloneEntry(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) ListAllLedger(_ context.Context, projectID string) ([]domainassets.LedgerEntry, error) {
	var out []domainassets.LedgerEntry
	for _, e := range s.ledger {
		if e.ProjectID == projectID {
			out = append(out, *cloneEntry(e))
		}
	}
	// 冻结时钟下 CreatedAt 全相同；不得按 ULID 打破平局（同毫秒 consume/expire
	// 可能排到 grant 前，quantity_after 链偶发断裂）。SliceStable 保留插入序。
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *memStore) Publish(_ context.Context, ev domainevents.Envelope) error {
	s.outbox = append(s.outbox, ev)
	return nil
}

// 适配接口：HoldingRepo / LedgerRepo 方法名与 memStore 不完全同名，用薄包装。
type memHoldings struct{ s *memStore }
type memLedger struct{ s *memStore }

func (r memHoldings) Insert(ctx context.Context, h *domainassets.Holding) error {
	return r.s.InsertHolding(ctx, h)
}
func (r memHoldings) GetByID(ctx context.Context, p, id string) (*domainassets.Holding, error) {
	return r.s.GetHoldingByID(ctx, p, id)
}
func (r memHoldings) GetByIDForUpdate(ctx context.Context, p, id string) (*domainassets.Holding, error) {
	return r.s.GetHoldingByIDForUpdate(ctx, p, id)
}
func (r memHoldings) ListForUpdate(ctx context.Context, p string, ot domainassets.OwnerType, oid, def string) ([]domainassets.Holding, error) {
	return r.s.ListForUpdate(ctx, p, ot, oid, def)
}
func (r memHoldings) ListByOwner(ctx context.Context, p string, ot domainassets.OwnerType, oid string, limit int, before time.Time) ([]domainassets.Holding, error) {
	return r.s.ListByOwner(ctx, p, ot, oid, limit, before)
}
func (r memHoldings) Update(ctx context.Context, h *domainassets.Holding, v int64) error {
	return r.s.UpdateHolding(ctx, h, v)
}
func (r memHoldings) Delete(ctx context.Context, p, id string, v int64) error {
	return r.s.DeleteHolding(ctx, p, id, v)
}
func (r memHoldings) ListExpiredInProject(ctx context.Context, projectID string, now time.Time, limit int) ([]domainassets.Holding, error) {
	return r.s.ListExpiredInProject(ctx, projectID, now, limit)
}
func (r memHoldings) ListAllInProject(ctx context.Context, p string) ([]domainassets.Holding, error) {
	return r.s.ListAllHoldings(ctx, p)
}

func (r memLedger) InsertIfAbsent(ctx context.Context, e *domainassets.LedgerEntry) (*domainassets.LedgerEntry, bool, error) {
	return r.s.InsertIfAbsent(ctx, e)
}
func (r memLedger) GetByIdempotencyKey(ctx context.Context, p, k string) (*domainassets.LedgerEntry, error) {
	return r.s.GetByIdempotencyKey(ctx, p, k)
}
func (r memLedger) ListByRef(ctx context.Context, p, rt, rid string) ([]domainassets.LedgerEntry, error) {
	return r.s.ListByRef(ctx, p, rt, rid)
}
func (r memLedger) ListByOwner(ctx context.Context, p string, ot domainassets.OwnerType, oid, def string, limit int, before time.Time) ([]domainassets.LedgerEntry, error) {
	return r.s.ListLedgerByOwner(ctx, p, ot, oid, def, limit, before)
}
func (r memLedger) ListAllInProject(ctx context.Context, p string) ([]domainassets.LedgerEntry, error) {
	return r.s.ListAllLedger(ctx, p)
}

func adminCtx(projectID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorKind:      domainshared.ActorKindService,
		CredentialType: domainshared.CredentialTypeAPIKey,
		ProjectID:      projectID,
		APIKeyID:       "k1",
		Permissions:    []string{"economy.write"},
	})
}

func userCtx(projectID, userID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorKind:      domainshared.ActorKindEndUser,
		CredentialType: domainshared.CredentialTypeToken,
		ProjectID:      projectID,
		UserID:         userID,
	})
}

type listProjectsStub struct {
	projects.Repository
	list []projects.Project
}

func (s *listProjectsStub) ListProjects(context.Context) ([]projects.Project, error) {
	return s.list, nil
}

type testEnv struct {
	assets *Assets
	store  *memStore
	now    time.Time
}

func setupAssets(t *testing.T) *testEnv {
	t.Helper()
	store := newMemStore()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	a := newAssets(store, store, memHoldings{store}, memLedger{store}, store, nil, &listProjectsStub{list: []projects.Project{{ID: "p1", Status: "active"}}})
	a.now = func() time.Time { return now }
	return &testEnv{assets: a, store: store, now: now}
}

func (e *testEnv) createDef(t *testing.T, class domainassets.Class, code string, opts ...func(*CreateDefCommand)) *domainassets.Def {
	return e.createDefFor(t, "p1", class, code, opts...)
}

func (e *testEnv) createDefFor(t *testing.T, projectID string, class domainassets.Class, code string, opts ...func(*CreateDefCommand)) *domainassets.Def {
	t.Helper()
	cmd := CreateDefCommand{Code: code, Name: code, Class: class, Tradable: class != domainassets.ClassEntitlement}
	for _, o := range opts {
		o(&cmd)
	}
	def, err := e.assets.CreateDef(adminCtx(projectID), cmd)
	require.NoError(t, err)
	return def
}

func TestCreateDef_CurrencyRejectsExpiresIn(t *testing.T) {
	env := setupAssets(t)
	ttl := int64(3600)
	_, err := env.assets.CreateDef(adminCtx("p1"), CreateDefCommand{
		Code: "gold", Name: "Gold", Class: domainassets.ClassCurrency, ExpiresIn: &ttl,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGrant_EntitlementRequiresExpiresAt(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassEntitlement, "vip")
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "vip", Quantity: 1, IdempotencyKey: "g1",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Empty(t, env.store.holdings)
	require.Empty(t, env.store.ledger)
}

func TestGrant_InstanceQuantityMustBeOne(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassInstance, "sword")
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "sword", Quantity: 2, IdempotencyKey: "g1",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	res, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "sword", Quantity: 1, IdempotencyKey: "g2",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Entries[0].QuantityAfter)
	require.Equal(t, 1, len(env.store.holdings))
}

func TestGrantConsume_StackBucketsAndFEFO(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassStack, "potion", func(c *CreateDefCommand) { c.Tradable = true })
	t1 := env.now.Add(time.Hour)
	t2 := env.now.Add(3 * time.Hour)
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "potion", Quantity: 10, ExpiresAt: &t1, IdempotencyKey: "g-early",
	})
	require.NoError(t, err)
	_, err = env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "potion", Quantity: 5, ExpiresAt: &t1, IdempotencyKey: "g-merge",
	})
	require.NoError(t, err)
	require.Equal(t, 1, countHoldings(env.store), "同到期应并桶")
	_, err = env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "potion", Quantity: 10, ExpiresAt: &t2, IdempotencyKey: "g-late",
	})
	require.NoError(t, err)
	require.Equal(t, 2, countHoldings(env.store), "不同到期应拆桶")

	res, err := env.assets.Consume(adminCtx("p1"), ConsumeCommand{
		OwnerID: "u1", DefCode: "potion", Quantity: 18, IdempotencyKey: "c-fefo",
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(res.Entries), 2)
	require.Equal(t, 1, countHoldings(env.store), "FEFO 先扣尽早到期桶")
	var remaining *domainassets.Holding
	for _, h := range env.store.holdings {
		remaining = h
	}
	require.NotNil(t, remaining)
	require.True(t, remaining.ExpiresAt.Equal(t2))
	require.Equal(t, int64(7), remaining.Quantity)
}

func TestConsume_InsufficientNoPartial(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 10, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	_, err = env.assets.Consume(adminCtx("p1"), ConsumeCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 11, IdempotencyKey: "c1",
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, 1, countHoldings(env.store))
	require.Equal(t, int64(10), anyHolding(env.store).Quantity)
	require.Equal(t, 1, len(env.store.ledger), "不足时不得落部分流水")
}

func TestGrant_MaxQuantityRejected(t *testing.T) {
	env := setupAssets(t)
	max := int64(10)
	env.createDef(t, domainassets.ClassCurrency, "gold", func(c *CreateDefCommand) { c.MaxQuantity = &max })
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 8, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	_, err = env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 3, IdempotencyKey: "g2",
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, int64(8), anyHolding(env.store).Quantity)
}

func TestGrant_IdempotentReplayNoExtraLedger(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	cmd := GrantCommand{OwnerID: "u1", DefCode: "gold", Quantity: 7, IdempotencyKey: "same"}
	first, err := env.assets.Grant(adminCtx("p1"), cmd)
	require.NoError(t, err)
	require.False(t, first.IdempotentReplay)
	second, err := env.assets.Grant(adminCtx("p1"), cmd)
	require.NoError(t, err)
	require.True(t, second.IdempotentReplay)
	require.Equal(t, 1, len(env.store.ledger))
	require.Equal(t, int64(7), anyHolding(env.store).Quantity)
}

func TestEndUserWriteDenied(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	_, err := env.assets.Grant(userCtx("p1", "u1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 1, IdempotencyKey: "g1",
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Empty(t, env.store.holdings)
}

func TestTransfer_BilateralEntriesShareRef(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold", func(c *CreateDefCommand) { c.Tradable = true })
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 20, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	res, err := env.assets.Transfer(adminCtx("p1"), TransferCommand{
		FromOwnerID: "u1", ToOwnerID: "u2", DefCode: "gold", Quantity: 5, IdempotencyKey: "t1",
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(res.Entries), 2)
	ref := res.Entries[0].RefID
	var kinds []string
	for _, e := range res.Entries {
		require.Equal(t, ref, e.RefID)
		kinds = append(kinds, string(e.Kind))
	}
	require.Contains(t, kinds, string(domainassets.KindTransferOut))
	require.Contains(t, kinds, string(domainassets.KindTransferIn))
	require.Equal(t, 2, countHoldings(env.store))
}

func TestReconcile_ZeroDriftAndTamper(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 15, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	_, err = env.assets.Consume(adminCtx("p1"), ConsumeCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 4, IdempotencyKey: "c1",
	})
	require.NoError(t, err)
	report, err := env.assets.Reconcile(adminCtx("p1"))
	require.NoError(t, err)
	require.True(t, report.ZeroDrift)
	require.Empty(t, report.Drifts)

	anyHolding(env.store).Quantity = 99
	report, err = env.assets.Reconcile(adminCtx("p1"))
	require.NoError(t, err)
	require.False(t, report.ZeroDrift)
	require.NotEmpty(t, report.Drifts)
}

func TestWithSystemPrincipal_IsSystemActorNotFakeAPIKey(t *testing.T) {
	t.Parallel()
	ctx := withSystemPrincipal(context.Background(), "p1")
	p, ok := contexts.Principal(ctx)
	require.True(t, ok)
	require.Equal(t, domainshared.ActorKindSystem, p.ActorKind)
	require.True(t, p.IsSystem())
	require.Empty(t, p.APIKeyID)
	require.NotEqual(t, domainshared.CredentialTypeAPIKey, p.CredentialType)
	require.Equal(t, "p1", p.ProjectID)
	require.NoError(t, requireAssetWrite(ctx))
}

func TestExpireDue_TwoProjectsIgnoresStickyPrincipal(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	a := newAssets(store, store, memHoldings{store}, memLedger{store}, store, nil, &listProjectsStub{list: []projects.Project{
		{ID: "p1", Status: "active"},
		{ID: "p2", Status: "active"},
	}})
	a.now = func() time.Time { return now }
	env := &testEnv{assets: a, store: store, now: now}

	env.createDefFor(t, "p1", domainassets.ClassStack, "ticket")
	env.createDefFor(t, "p2", domainassets.ClassStack, "ticket")
	past := now.Add(-time.Hour)
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "ticket", Quantity: 3, ExpiresAt: &past, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	_, err = env.assets.Grant(adminCtx("p2"), GrantCommand{
		OwnerID: "u1", DefCode: "ticket", Quantity: 2, ExpiresAt: &past, IdempotencyKey: "g2",
	})
	require.NoError(t, err)
	require.Equal(t, 1, countHoldingsIn(store, "p1"))
	require.Equal(t, 1, countHoldingsIn(store, "p2"))

	n, err := env.assets.ExpireDue(adminCtx("p1"), now)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
	require.Equal(t, 0, countHoldingsIn(store, "p1"))
	require.Equal(t, 0, countHoldingsIn(store, "p2"))
}

func TestExpireDue_DeletesAndLedger(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassStack, "ticket")
	past := env.now.Add(-time.Hour)
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "ticket", Quantity: 3, ExpiresAt: &past, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	require.Equal(t, 1, countHoldings(env.store))
	n, err := env.assets.ExpireDue(adminCtx("p1"), env.now)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	require.Empty(t, env.store.holdings)
	var kinds []string
	for _, e := range env.store.ledger {
		kinds = append(kinds, string(e.Kind))
	}
	require.Contains(t, kinds, string(domainassets.KindExpire))
	report, err := env.assets.Reconcile(adminCtx("p1"))
	require.NoError(t, err)
	require.True(t, report.ZeroDrift)
}

func TestGrantRollback_SameTx(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	err := env.store.RunInTx(adminCtx("p1"), func(txCtx context.Context) error {
		_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
			OwnerID: "u1", DefCode: "gold", Quantity: 9, IdempotencyKey: "g1",
		})
		require.NoError(t, err)
		require.Equal(t, 1, countHoldings(env.store))
		return errors.New("boom")
	})
	require.EqualError(t, err, "boom")
	require.Empty(t, env.store.holdings)
	require.Empty(t, env.store.ledger)
}

func TestOrderFulfiller_TopupGrantsCurrency(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	f := NewOrderFulfiller(env.assets)
	order := &domainpayments.Order{
		ID: "ord-1", ProjectID: "p1", UserID: "u1", Amount: 42,
		PurposeKind: domainpayments.PurposeTopup,
		Purpose:     json.RawMessage(`{"currency_code":"gold","amount":42}`),
	}
	ref, err := f.Fulfill(context.Background(), order)
	require.NoError(t, err)
	require.NotEmpty(t, ref)
	require.Equal(t, int64(42), anyHolding(env.store).Quantity)
	require.Equal(t, "order", env.store.ledger[0].RefType)
	require.Equal(t, "ord-1", env.store.ledger[0].RefID)
}

func TestMutate_InstanceLevel(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassInstance, "sword", func(c *CreateDefCommand) { c.Upgradeable = true })
	res, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "sword", Quantity: 1, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	holdingID := res.Entries[0].HoldingID
	lvl := int32(3)
	_, err = env.assets.Mutate(adminCtx("p1"), MutateCommand{
		HoldingID: holdingID, Level: &lvl, IdempotencyKey: "m1",
	})
	require.NoError(t, err)
	require.Equal(t, int32(3), env.store.holdings[holdingID].Level)
}

func TestMapWriteError_PreservesInvalidArgumentMessages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		code codes.Code
		msg  string
	}{
		{domainassets.ErrSameOwner, codes.InvalidArgument, "cannot transfer to the same owner"},
		{domainassets.ErrTransferOwnersRequired, codes.InvalidArgument, "from_owner_id and to_owner_id are required"},
		{domainassets.ErrHoldingIDRequired, codes.InvalidArgument, "holding_id is required"},
		{domainassets.ErrOwnerRequired, codes.InvalidArgument, "owner_id is required"},
		{domainassets.ErrInvalidCode, codes.InvalidArgument, "def code must match ^[a-z][a-z0-9_]{0,63}$"},
		{domainassets.ErrIdempotencyTooLong, codes.InvalidArgument, "idempotency_key exceeds 128 characters"},
		{domainassets.ErrIdempotencyRequired, codes.InvalidArgument, domainassets.ErrIdempotencyRequired.Error()},
		{domainassets.ErrProjectRequired, codes.Unauthenticated, "missing project context"},
	}
	for _, tc := range cases {
		got := mapWriteError(tc.err)
		require.Equal(t, tc.code, status.Code(got), tc.msg)
		require.Equal(t, tc.msg, status.Convert(got).Message())
	}
}

func TestGrantTransfer_InvalidArgumentMessages(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold", func(c *CreateDefCommand) { c.Tradable = true })
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "1bad", Quantity: 1, IdempotencyKey: "g1",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, "def code must match ^[a-z][a-z0-9_]{0,63}$", status.Convert(err).Message())

	_, err = env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "", DefCode: "gold", Quantity: 1, IdempotencyKey: "g2",
	})
	require.Equal(t, "owner_id is required", status.Convert(err).Message())

	_, err = env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 1, IdempotencyKey: strings.Repeat("x", domainassets.MaxIdempotencyKey+1),
	})
	require.Equal(t, "idempotency_key exceeds 128 characters", status.Convert(err).Message())

	_, err = env.assets.Transfer(adminCtx("p1"), TransferCommand{
		FromOwnerID: "u1", ToOwnerID: "u1", DefCode: "gold", Quantity: 1, IdempotencyKey: "t1",
	})
	require.Equal(t, "cannot transfer to the same owner", status.Convert(err).Message())

	_, err = env.assets.Transfer(adminCtx("p1"), TransferCommand{
		FromOwnerID: "", ToOwnerID: "u2", DefCode: "gold", Quantity: 1, IdempotencyKey: "t2",
	})
	require.Equal(t, "from_owner_id and to_owner_id are required", status.Convert(err).Message())

	_, err = env.assets.Mutate(adminCtx("p1"), MutateCommand{HoldingID: "", IdempotencyKey: "m1"})
	require.Equal(t, "holding_id is required", status.Convert(err).Message())
}

func countHoldings(s *memStore) int { return len(s.holdings) }

func countHoldingsIn(s *memStore, projectID string) int {
	n := 0
	for _, h := range s.holdings {
		if h.ProjectID == projectID {
			n++
		}
	}
	return n
}

func anyHolding(s *memStore) *domainassets.Holding {
	for _, h := range s.holdings {
		return h
	}
	return nil
}

// TestOrderFulfiller_TopupQuantityFromOrderAmount（A2）：topup 履约数量一律取
// Order.Amount，不信任 purpose.amount（建单已校验相等，此处为纵深防御）。
func TestOrderFulfiller_TopupQuantityFromOrderAmount(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	f := NewOrderFulfiller(env.assets)
	order := &domainpayments.Order{
		ID: "ord-topup-src", ProjectID: "p1", UserID: "u1", Amount: 1,
		PurposeKind: domainpayments.PurposeTopup,
		Purpose:     json.RawMessage(`{"currency_code":"gold","amount":999999}`),
	}
	_, err := f.Fulfill(context.Background(), order)
	require.NoError(t, err)
	require.Equal(t, int64(1), anyHolding(env.store).Quantity)
}
