package subscriptions

import (
	"context"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
)

func requireServerWrite(ctx context.Context) error {
	return appshared.RequireServerPrincipal(ctx)
}

// withSystemPrincipal 为 worker / 履约注入 system 主体（资产写路径 requireAssetWrite）。
func withSystemPrincipal(ctx context.Context, projectID string) context.Context {
	if p, ok := contexts.Principal(ctx); ok && p != nil && (p.IsSystem() || p.ActorKind == shared.ActorKindService) {
		if p.ProjectID == "" && projectID != "" {
			cp := *p
			cp.ProjectID = projectID
			return contexts.WithPrincipal(ctx, &cp)
		}
		return ctx
	}
	return contexts.WithPrincipal(ctx, shared.NewSystemPrincipal(projectID))
}
