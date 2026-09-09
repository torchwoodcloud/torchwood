package assets

import (
	"context"

	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
)

// LiveHoldingForUpdate 返回业主某定义下未过期的第一行持有（FOR UPDATE）。
// 供订阅 entitlement 续期：已有持有则 Mutate，否则 Grant。须在外层事务内调用。
func (a *Assets) LiveHoldingForUpdate(ctx context.Context, ownerID, defCode string) (*domainassets.Holding, error) {
	if err := requireAssetWrite(ctx); err != nil {
		return nil, err
	}
	scope, err := a.writeScope(ctx)
	if err != nil {
		return nil, err
	}
	h, err := domainassets.LookupLiveHolding(a.svc, ctx, scope, ownerID, defCode)
	if err != nil {
		return nil, mapWriteError(err)
	}
	return h, nil
}
