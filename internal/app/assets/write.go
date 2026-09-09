package assets

import (
	"context"

	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
)

type (
	OpResult        = domainassets.OpResult
	GrantCommand    = domainassets.GrantCommand
	ConsumeCommand  = domainassets.ConsumeCommand
	TransferCommand = domainassets.TransferCommand
	MutateCommand   = domainassets.MutateCommand
	ExpireCommand   = domainassets.ExpireCommand
)

func (a *Assets) writeScope(ctx context.Context) (domainassets.Scope, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return domainassets.Scope{}, err
	}
	return domainassets.Scope{ProjectID: projectID, Operator: operatorFrom(ctx)}, nil
}

func (a *Assets) doWrite(
	ctx context.Context,
	kind domainassets.EntryKind,
	class string,
	fn func(domainassets.Scope) (*domainassets.OpResult, error),
) (*OpResult, error) {
	if err := requireAssetWrite(ctx); err != nil {
		return nil, err
	}
	scope, err := a.writeScope(ctx)
	if err != nil {
		return nil, err
	}
	res, err := fn(scope)
	replay := res != nil && res.IdempotentReplay
	a.observe(kind, class, err, replay)
	if res == nil {
		res = &OpResult{}
	}
	return res, mapWriteError(err)
}

// Grant 发放资产（幂等键必填）。
func (a *Assets) Grant(ctx context.Context, cmd GrantCommand) (*OpResult, error) {
	return a.doWrite(ctx, domainassets.KindGrant, cmd.DefCode, func(scope domainassets.Scope) (*OpResult, error) {
		return a.svc.Grant(ctx, scope, cmd)
	})
}

// Consume 按 FEFO 扣桶；数量不足整体失败。
func (a *Assets) Consume(ctx context.Context, cmd ConsumeCommand) (*OpResult, error) {
	return a.doWrite(ctx, domainassets.KindConsume, cmd.DefCode, func(scope domainassets.Scope) (*OpResult, error) {
		return a.svc.Consume(ctx, scope, cmd)
	})
}

// Transfer 原子转让（仅 tradable 定义；entitlement 禁止）。
func (a *Assets) Transfer(ctx context.Context, cmd TransferCommand) (*OpResult, error) {
	return a.doWrite(ctx, domainassets.KindTransferOut, cmd.DefCode, func(scope domainassets.Scope) (*OpResult, error) {
		return a.svc.Transfer(ctx, scope, cmd)
	})
}

// Mutate 变更实例/权益属性（level / metadata / expires_at）。
func (a *Assets) Mutate(ctx context.Context, cmd MutateCommand) (*OpResult, error) {
	return a.doWrite(ctx, domainassets.KindMutate, "", func(scope domainassets.Scope) (*OpResult, error) {
		return a.svc.Mutate(ctx, scope, cmd)
	})
}

// Expire 强制失效一行持有（删行 + expire 流水）。
func (a *Assets) Expire(ctx context.Context, cmd ExpireCommand) (*OpResult, error) {
	return a.doWrite(ctx, domainassets.KindExpire, "", func(scope domainassets.Scope) (*OpResult, error) {
		return a.svc.Expire(ctx, scope, cmd)
	})
}

func (a *Assets) observe(kind domainassets.EntryKind, class string, err error, replay bool) {
	result := "ok"
	if err != nil {
		result = "error"
	} else if replay {
		result = "idempotent"
	}
	if class == "" {
		class = "unknown"
	}
	assetOpsTotal.WithLabelValues(string(kind), class, result).Inc()
}
