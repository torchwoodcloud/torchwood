package shared

import (
	"context"

	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RequirePlatformAdmin 拒绝非平台 admin 主体（API key、受限 console 管理员、
// 端用户、匿名）。供各 use-case 做纵深防御（fail-closed）：即使绕过拦截器
// 直接调用 use-case，平台级敏感写操作（Functions 写方法、API Key 管理、
// 用户密码/令牌/删除、项目创建等）也必须有平台 admin 凭证。
// 注意：Databases schema DDL 自 Round3 H3 起与 G12 Functions 同口径使用
// RequireServerWriteActor（API key 持 databases.write 可做 DDL），不在本守卫内。
func RequirePlatformAdmin(ctx context.Context) error {
	principal, ok := contexts.Principal(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if principal.ActorKind != shared.ActorKindAdmin || !principal.IsPlatformAdmin {
		return status.Error(codes.PermissionDenied, "platform admin required")
	}
	return nil
}

// RequireAdminActor 拒绝非 console admin 会话主体（API key/端用户/匿名）。
// 角色级细粒度（viewer/member/owner/admin）由拦截器 permission 门禁把关；
// 本守卫仅保证调用者是 admin 会话 actor（对齐 consolegrpc.requireAdminActor）。
func RequireAdminActor(ctx context.Context) error {
	principal, ok := contexts.Principal(ctx)
	if !ok || principal.ActorKind != shared.ActorKindAdmin {
		return status.Error(codes.PermissionDenied, "console admin session required")
	}
	return nil
}

// RequireServerWriteActor 校验调用者具备经 Server API 调用业务写方法的资格
// （纵深防御第二层）：console admin 会话（ActorKind=admin，角色细粒度由
// 拦截器 adminRoleMethodRules 把关）或 API key 主体（ActorKind=service，
// scope 细粒度由拦截器 PolicySet.AllowsAPIKey 把关）。匿名与端用户一律拒绝——
// use-case 直接调用（绕过拦截器）时不得以 SystemPrincipal 执行写操作。
func RequireServerWriteActor(ctx context.Context) error {
	principal, ok := contexts.Principal(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
	switch principal.ActorKind {
	case shared.ActorKindAdmin, shared.ActorKindService:
		return nil
	default:
		return status.Error(codes.PermissionDenied, "server api write not allowed for this principal")
	}
}
