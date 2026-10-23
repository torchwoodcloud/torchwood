package interceptor

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/lynx-go/grpcapi/authz"
	"github.com/lynx-go/grpcapi/policy"
	"google.golang.org/grpc"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// newTestSet 按三桶语义构造可求值策略集（单测接线用，域 auth 树与生产
// legacyProvider 同构——domainauth.NewSet）。真实策略（proto 声明 →
// AssertSemantic）的语义断言由 cmd/server/internal/runtime 测试与矩阵测试
// （Phase B）承担；此处只验证拦截器执行逻辑本身。
//
//	server: SERVER 面方法（key 通道 = users.write scope；admin 会话角色门
//	        为不限角色——scopeless SERVER 在规范树中非法，生产面由
//	        AssertSemantic 保证恒有 scope）
//	perms:  PERMISSION/END_USER 面 method → 角色集
func newTestSet(public []string, server []string, perms map[string][]string) (*policy.Set, error) {
	policies := make([]domainauth.MethodPolicy, 0, len(public)+len(server)+len(perms))
	for _, m := range public {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessPublic})
	}
	for _, m := range server {
		policies = append(policies, domainauth.MethodPolicy{
			Method: m, Service: serviceOf(m), Access: domainauth.AccessServer,
			Scope: &domainauth.ScopeRule{Resource: string(domainauth.ScopeUsers), Op: domainauth.ScopeWrite},
		})
	}
	for m, roles := range perms {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessPermission, Permissions: roles})
	}
	return domainauth.NewSet(policies...)
}

// newTestInterceptor 同 newTestSet 并构造 authn 拦截器；返回的 pair 经
// invokeChain 按生产链序（authn → 库 authz → handler）执行——档位/角色/
// scope 执行在库 authz 拦截器。
func newTestInterceptor(v stubValidator, public []string, server []string, perms map[string][]string) (*AuthInterceptor, *policy.Set, error) {
	set, err := newTestSet(public, server, perms)
	if err != nil {
		return nil, nil, err
	}
	ic, err := NewAuthInterceptor(v, set)
	if err != nil {
		return nil, nil, err
	}
	return ic, set, nil
}

// newTestInterceptorRoles 同上，SERVER 方法带 admin 角色门与 key scope
// （角色门为 serverRoles；scope 固定 users.write 供 key 通道用例）。
func newTestInterceptorRoles(v stubValidator, public []string, serverRoles map[string][]string, perms map[string][]string) (*AuthInterceptor, *policy.Set, error) {
	policies := make([]domainauth.MethodPolicy, 0, len(public)+len(serverRoles)+len(perms))
	for _, m := range public {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessPublic})
	}
	for m, roles := range serverRoles {
		policies = append(policies, domainauth.MethodPolicy{
			Method: m, Service: serviceOf(m), Access: domainauth.AccessServer,
			Scope:      &domainauth.ScopeRule{Resource: string(domainauth.ScopeUsers), Op: domainauth.ScopeWrite},
			AdminRoles: roles,
		})
	}
	for m, roles := range perms {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessPermission, Permissions: roles})
	}
	set, err := domainauth.NewSet(policies...)
	if err != nil {
		return nil, nil, err
	}
	ic, err := NewAuthInterceptor(v, set)
	if err != nil {
		return nil, nil, err
	}
	return ic, set, nil
}

// invokeChain 按生产链序执行 authn → 库 authz → handler（与
// cmd/server/internal/runtime 装配一致，含拒绝审计回调；库拦截器自身日志
// 静音，拒绝留痕经 OnDecision 进 authn 拦截器的 logger/sink）。
func invokeChain(t testing.TB, ic *AuthInterceptor, set *policy.Set, ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	t.Helper()
	az, err := authz.NewInterceptor(set, nil, authz.Options{
		OnDecision: ic.AuthzOnDecision(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ic.UnaryAuthMiddleware(ctx, req, info, func(ctx context.Context, req any) (any, error) {
		return az.Unary()(ctx, req, info, handler)
	})
}

func serviceOf(fullMethod string) string {
	for i := len(fullMethod) - 1; i >= 0; i-- {
		if fullMethod[i] == '/' {
			return fullMethod[:i]
		}
	}
	return fullMethod
}

// mustPolicySet 构造含 ListUsers（SERVER + users.read）的最小策略集
// （failingValidator 类用例：凭证校验失败发生在策略求值之前）。
func mustPolicySet() *policy.Set {
	set, err := domainauth.NewSet([]domainauth.MethodPolicy{
		{
			Method: "/torchwood.server.v1.UsersService/ListUsers", Service: "/torchwood.server.v1.UsersService",
			Access: domainauth.AccessServer,
			Scope:  &domainauth.ScopeRule{Resource: string(domainauth.ScopeUsers), Op: domainauth.ScopeRead},
		},
	}...)
	if err != nil {
		panic(err)
	}
	return set
}
