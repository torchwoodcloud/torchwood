package assets

import (
	"context"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// requireSystemActor 断言内部 System 主体（worker / 支付履约），匿名拒绝。
func requireSystemActor(ctx context.Context) error {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if p.ActorKind != shared.ActorKindSystem {
		return status.Error(codes.PermissionDenied, "system principal required")
	}
	return nil
}

// requireAssetWrite 断言资产写路径主体（红线 D6）：console admin 会话、
// API key（RequireServerPrincipal）或 System（requireSystemActor，worker /
// 支付履约）的组合放行。终端用户一律 PermissionDenied——use-case 直接调用
// 也不得写资产。错误文案保持资产写路径语义（Unauthenticated 原样透传）。
func requireAssetWrite(ctx context.Context) error {
	if err := appshared.RequireAnyOf(ctx, appshared.RequireServerPrincipal, requireSystemActor); err != nil {
		if status.Code(err) == codes.PermissionDenied {
			return status.Error(codes.PermissionDenied, "asset write not allowed for this principal")
		}
		return err
	}
	return nil
}

// withSystemPrincipal 为 worker / 支付履约注入 System 主体（仍走 requireAssetWrite）。
func withSystemPrincipal(ctx context.Context, projectID string) context.Context {
	if p, ok := contexts.Principal(ctx); ok && p != nil && (p.IsSystem() || p.ActorKind == shared.ActorKindService) {
		if p.ProjectID == "" {
			cp := *p
			cp.ProjectID = projectID
			return contexts.WithPrincipal(ctx, &cp)
		}
		return ctx
	}
	return contexts.WithPrincipal(ctx, shared.NewSystemPrincipal(projectID))
}
