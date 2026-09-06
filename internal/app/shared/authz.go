package shared

import (
	"context"

	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RequireEndUser 拒绝非端用户主体（admin/API key/匿名）。纯 actor 语义守卫：
// 仅要求 ActorKindEndUser 且 UserID 非空；项目绑定等更细的上下文校验由
// 调用面自行把关（如 client 包 dbPrincipal），不在本守卫重复。
// 与 RequireServerPrincipal/RequirePlatformPrincipal/RequireConsolePrincipal
// 共同构成 use-case 层四守卫体系（机制 v8 评审 B1 裁决），供 RequireAnyOf 组合。
func RequireEndUser(ctx context.Context) error {
	principal, ok := contexts.Principal(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if principal.ActorKind != shared.ActorKindEndUser || principal.UserID == "" {
		return status.Error(codes.PermissionDenied, "end user required")
	}
	return nil
}

// RequirePlatformPrincipal 拒绝非平台 admin 主体（API key、受限 console 管理员、
// 端用户、匿名）。供各 use-case 做纵深防御（fail-closed）：即使绕过拦截器
// 直接调用 use-case，平台级敏感写操作（Functions 写方法、API Key 管理、
// 用户密码/令牌/删除、项目创建等）也必须有平台 admin 凭证。
// 注意：Databases schema DDL 自 Round3 H3 起与 G12 Functions 同口径使用
// RequireServerPrincipal（API key 持 databases.write 可做 DDL），不在本守卫内。
func RequirePlatformPrincipal(ctx context.Context) error {
	principal, ok := contexts.Principal(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if principal.ActorKind != shared.ActorKindAdmin || !principal.IsPlatformAdmin {
		return status.Error(codes.PermissionDenied, "platform admin required")
	}
	return nil
}

// RequireConsolePrincipal 拒绝非 console admin 会话主体（API key/端用户/匿名）。
// 角色级细粒度（viewer/member/owner/admin）由拦截器 permission 门禁把关；
// 本守卫仅保证调用者是 admin 会话 actor（对齐 consolegrpc.requireAdminActor）。
func RequireConsolePrincipal(ctx context.Context) error {
	principal, ok := contexts.Principal(ctx)
	if !ok || principal.ActorKind != shared.ActorKindAdmin {
		return status.Error(codes.PermissionDenied, "console admin session required")
	}
	return nil
}

// RequireServerPrincipal 校验调用者具备经 Server API 调用业务写方法的资格
// （纵深防御第二层）：console admin 会话（ActorKind=admin，角色细粒度由
// 拦截器 adminRoleMethodRules 把关）或 API key 主体（ActorKind=service，
// scope 细粒度由拦截器 PolicySet.AllowsAPIKey 把关）。匿名与端用户一律拒绝——
// use-case 直接调用（绕过拦截器）时不得以 SystemPrincipal 执行写操作。
func RequireServerPrincipal(ctx context.Context) error {
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

// RequireAnyOf 组合守卫：依次执行 guards，任一通过即放行（机制 v8 评审 B1）。
// 全部失败时的错误聚合：优先返回第一个非 Unauthenticated 错误（已认证主体
// 得到 PermissionDenied 级语义），全部为 Unauthenticated 时返回 Unauthenticated
// （匿名主体不被后续守卫的 PermissionDenied 掩盖）。
func RequireAnyOf(ctx context.Context, guards ...func(context.Context) error) error {
	if len(guards) == 0 {
		return status.Error(codes.Internal, "require any of: no guards configured")
	}
	var denied error
	for _, guard := range guards {
		err := guard(ctx)
		if err == nil {
			return nil
		}
		if status.Code(err) == codes.Unauthenticated {
			continue
		}
		if denied == nil {
			denied = err
		}
	}
	if denied != nil {
		return denied
	}
	return status.Error(codes.Unauthenticated, "unauthenticated")
}
