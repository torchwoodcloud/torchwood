package interceptor

import (
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// newTestInterceptor 按三桶语义构造小策略注册表（单测接线用）。
// 真实策略（proto 声明 → AssertSemantic）的语义断言由 internal/runtime
// 测试与矩阵测试（Phase B）承担；此处只验证拦截器执行逻辑本身。
//
//	server: SERVER 面方法（不开放 key 通道——AllowsAPIKey fail-closed；
//	        admin 会话角色门为不限角色）
//	perms:  PERMISSION/END_USER 面 method → 角色集
func newTestInterceptor(v stubValidator, public []string, server []string, perms map[string][]string) (*AuthInterceptor, error) {
	policies := make([]domainauth.MethodPolicy, 0, len(public)+len(server)+len(perms))
	for _, m := range public {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessPublic})
	}
	for _, m := range server {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessServer})
	}
	for m, roles := range perms {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessPermission, Permissions: roles})
	}
	set, err := domainauth.NewPolicySet(policies)
	if err != nil {
		return nil, err
	}
	return NewAuthInterceptor(v, set)
}

// newTestInterceptorRoles 同上，SERVER 方法带 admin 角色门与 key scope
// （角色门为 serverRoles；scope 固定 users.write 供 key 通道用例）。
func newTestInterceptorRoles(v stubValidator, public []string, serverRoles map[string][]domainauth.AdminRole, perms map[string][]string) (*AuthInterceptor, error) {
	policies := make([]domainauth.MethodPolicy, 0, len(public)+len(serverRoles)+len(perms))
	for _, m := range public {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessPublic})
	}
	for m, roles := range serverRoles {
		policies = append(policies, domainauth.MethodPolicy{
			Method: m, Service: serviceOf(m), Access: domainauth.AccessServer,
			Scope:      &domainauth.ScopeRule{Resource: domainauth.ScopeUsers, Op: domainauth.ScopeWrite},
			AdminRoles: roles,
		})
	}
	for m, roles := range perms {
		policies = append(policies, domainauth.MethodPolicy{Method: m, Service: serviceOf(m), Access: domainauth.AccessPermission, Permissions: roles})
	}
	set, err := domainauth.NewPolicySet(policies)
	if err != nil {
		return nil, err
	}
	return NewAuthInterceptor(v, set)
}

func serviceOf(fullMethod string) string {
	for i := len(fullMethod) - 1; i >= 0; i-- {
		if fullMethod[i] == '/' {
			return fullMethod[:i]
		}
	}
	return fullMethod
}

// mustPolicySet 构造含 ListUsers（SERVER）的最小注册表（failingValidator
// 类用例：凭证校验失败发生在策略放行之后）。
func mustPolicySet() *domainauth.PolicySet {
	set, err := domainauth.NewPolicySet([]domainauth.MethodPolicy{
		{Method: "/torchwood.server.v1.UsersService/ListUsers", Service: "/torchwood.server.v1.UsersService", Access: domainauth.AccessServer},
	})
	if err != nil {
		panic(err)
	}
	return set
}
