package assets

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
)

// Service 是资产五动词的领域服务：跨 Def + Holding + Ledger 的不变式引擎。
// 锁策略留在本类型内部；仓储只持久化。写路径走 uow.Run；实现可从 ctx 读取连接。
type Service struct {
	db       uow.Runner
	defs     DefRepo
	holdings HoldingRepo
	ledger   LedgerRepo
	events   shared.EventPublisher
	now      func() time.Time
	newID    func() string
}

// NewService 构造领域服务。now / newID 由 app 注入（测试可冻结时钟与 ID）。
func NewService(
	db uow.Runner,
	defs DefRepo,
	holdings HoldingRepo,
	ledger LedgerRepo,
	events shared.EventPublisher,
	now func() time.Time,
	newID func() string,
) *Service {
	s := &Service{
		db:       db,
		defs:     defs,
		holdings: holdings,
		ledger:   ledger,
		events:   events,
		now:      now,
		newID:    newID,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = func() string { panic("assets: newID is required") }
	}
	return s
}

func (s *Service) ts() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

func (s *Service) requireScope(scope Scope) error {
	if scope.ProjectID == "" {
		return ErrProjectRequired
	}
	return nil
}

func (s *Service) prepareWrite(ownerType OwnerType, ownerID, idem string) (OwnerType, string, error) {
	ot, err := NormalizeOwnerType(ownerType)
	if err != nil {
		return "", "", err
	}
	if ownerID == "" {
		return "", "", ErrOwnerRequired
	}
	key, err := ValidateIdempotencyKey(idem)
	if err != nil {
		return "", "", err
	}
	return ot, key, nil
}

func (s *Service) requireActiveDef(ctx context.Context, projectID, code string) (*Def, error) {
	def, err := s.defs.GetByCodeForShare(ctx, projectID, code)
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, ErrDefNotFound
	}
	if def.Status != DefStatusActive {
		return nil, ErrDefArchived
	}
	return def, nil
}

func (s *Service) resolveGrantExpiry(def *Def, explicit *time.Time) (*time.Time, error) {
	if explicit != nil {
		if !def.AllowsExpiry() {
			return nil, fmt.Errorf("%w: currency grant must not set expires_at", ErrMatrix)
		}
		return normalizeExpiry(explicit), nil
	}
	if def.ExpiresIn != nil && *def.ExpiresIn > 0 {
		if !def.AllowsExpiry() {
			return nil, fmt.Errorf("%w: currency must not have expires_in", ErrMatrix)
		}
		t := s.ts().Add(time.Duration(*def.ExpiresIn) * time.Second)
		return normalizeExpiry(&t), nil
	}
	if def.RequiresExpiry() {
		return nil, ErrExpiresAtRequired
	}
	return nil, nil
}

// loadReplay 按幂等键查重放流水。幂等键为项目级全局键空间（与 Stripe 语义
// 一致）：命中即返回该键首笔流水，**不区分动词**——因此不同动词不可复用
// 同一键，否则后到的动词会拿到另一动词的重放结果而非执行自身操作。内部
// 受控例外：订阅 benefits 的 entitlement 键在同一 period 内有意以
// Grant→Mutate 跨动词复用实现同周期幂等重放（app/subscriptions/benefits.go）。
func (s *Service) loadReplay(ctx context.Context, projectID, key string) ([]LedgerEntry, bool, error) {
	first, err := s.ledger.GetByIdempotencyKey(ctx, projectID, key)
	if err != nil {
		return nil, false, err
	}
	if first == nil {
		return nil, false, nil
	}
	if first.RefID != "" {
		all, err := s.ledger.ListByRef(ctx, projectID, first.RefType, first.RefID)
		if err != nil {
			return nil, false, err
		}
		if len(all) > 0 {
			return all, true, nil
		}
	}
	return []LedgerEntry{*first}, true, nil
}

func (s *Service) newEntry(e LedgerEntry, op json.RawMessage) *LedgerEntry {
	if e.ID == "" {
		e.ID = s.newID()
	}
	if e.Operator == nil {
		if len(op) == 0 {
			e.Operator = MarshalOperator(OperatorSnapshot{IsSystem: true})
		} else {
			e.Operator = op
		}
	}
	return &e
}

func (s *Service) publish(ctx context.Context, def *Def, ownerID string, kind EntryKind, delta, after int64, now time.Time) error {
	if s.events == nil {
		return nil
	}
	attrs := map[string]any{
		"def_id":         def.ID,
		"def_code":       def.Code,
		"class":          string(def.Class),
		"owner_id":       ownerID,
		"kind":           string(kind),
		"delta":          delta,
		"quantity_after": after,
	}
	return s.events.Publish(ctx, domainevents.Envelope{
		EventID:   s.newID(),
		Event:     EventNameForKind(kind),
		ProjectID: def.ProjectID,
		Domain:    EventDomain,
		Channel:   AccountsChannel(ownerID),
		CreatedAt: now,
		Attrs:     attrs,
	})
}

func (s *Service) normalizeRef(refType, refID, fallback string) (string, string) {
	if refType == "" {
		refType = fallback
	}
	if refID == "" {
		refID = s.newID()
	}
	return refType, refID
}

// LookupLiveHolding 给 app.LiveHoldingForUpdate 用：未过期第一行，事务内加锁。
// 不是 Service 方法；支付/订阅应走 app 封装，不直接调 HoldingRepo。
func LookupLiveHolding(s *Service, ctx context.Context, scope Scope, ownerID, defCode string) (*Holding, error) {
	return s.liveHolding(ctx, scope, ownerID, defCode)
}

func (s *Service) liveHolding(ctx context.Context, scope Scope, ownerID, defCode string) (*Holding, error) {
	if err := s.requireScope(scope); err != nil {
		return nil, err
	}
	if ownerID == "" {
		return nil, ErrOwnerRequired
	}
	code, err := ValidateCode(defCode)
	if err != nil {
		return nil, err
	}
	def, err := s.requireActiveDef(ctx, scope.ProjectID, code)
	if err != nil {
		return nil, err
	}
	holdings, err := s.holdings.ListForUpdate(ctx, scope.ProjectID, OwnerTypeUser, ownerID, def.ID)
	if err != nil {
		return nil, err
	}
	live := liveHoldings(holdings, s.ts())
	if len(live) == 0 {
		return nil, nil
	}
	h := live[0]
	return &h, nil
}

func normalizeExpiry(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC().Truncate(time.Microsecond)
	return &u
}

func sameExpiry(a, b *time.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(*b)
}

func liveHoldings(in []Holding, now time.Time) []Holding {
	out := make([]Holding, 0, len(in))
	for i := range in {
		if in[i].Expired(now) {
			continue
		}
		out = append(out, in[i])
	}
	return out
}

func checkMaxQuantity(def *Def, live []Holding, add int64) error {
	if def.MaxQuantity == nil {
		return nil
	}
	var sum int64
	for i := range live {
		sum += live[i].Quantity
	}
	if sum+add > *def.MaxQuantity {
		return fmt.Errorf("%w: %d + %d > %d", ErrMaxQuantity, sum, add, *def.MaxQuantity)
	}
	return nil
}
