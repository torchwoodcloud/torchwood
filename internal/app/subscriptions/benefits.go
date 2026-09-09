package subscriptions

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
)

// fulfillBenefits 在当前事务内调资产 Grant/Mutate（设计 §3.2）。
// entitlement 已有持有则 Mutate 延长 expires_at，不产生第二条持有。
func (s *Subscriptions) fulfillBenefits(ctx context.Context, sub *domainsubs.Subscription, periodEnd time.Time) error {
	if s.assets == nil {
		return fmt.Errorf("subscriptions: assets system is required for benefit fulfillment")
	}
	ctx = withSystemPrincipal(ctx, sub.ProjectID)
	periodKey := strconv.FormatInt(sub.CurrentPeriodStart.UTC().Unix(), 10)
	now := s.ts()

	for i, g := range sub.Benefits.Grants {
		var exp *time.Time
		if g.ExpiresIn != nil && *g.ExpiresIn > 0 {
			t := now.Add(time.Duration(*g.ExpiresIn) * time.Second)
			exp = &t
		}
		if _, err := s.assets.Grant(ctx, appassets.GrantCommand{
			OwnerType:      domainassets.OwnerTypeUser,
			OwnerID:        sub.UserID,
			DefCode:        g.AssetCode,
			Quantity:       g.Quantity,
			ExpiresAt:      exp,
			IdempotencyKey: fmt.Sprintf("sub:%s:grant:%s:%d", sub.ID, periodKey, i),
			RefType:        "subscription",
			RefID:          sub.ID,
		}); err != nil {
			return err
		}
	}

	for i, e := range sub.Benefits.Entitlements {
		key := fmt.Sprintf("sub:%s:ent:%s:%d", sub.ID, periodKey, i)
		h, err := s.assets.LiveHoldingForUpdate(ctx, sub.UserID, e.AssetCode)
		if err != nil {
			return err
		}
		if h != nil {
			level := e.Tier
			if _, err := s.assets.Mutate(ctx, appassets.MutateCommand{
				HoldingID:      h.ID,
				Level:          &level,
				ExpiresAt:      &periodEnd,
				IdempotencyKey: key,
				RefType:        "subscription",
				RefID:          sub.ID,
			}); err != nil {
				return err
			}
			continue
		}
		if _, err := s.assets.Grant(ctx, appassets.GrantCommand{
			OwnerType:      domainassets.OwnerTypeUser,
			OwnerID:        sub.UserID,
			DefCode:        e.AssetCode,
			Quantity:       1,
			ExpiresAt:      &periodEnd,
			Level:          e.Tier,
			IdempotencyKey: key,
			RefType:        "subscription",
			RefID:          sub.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}
