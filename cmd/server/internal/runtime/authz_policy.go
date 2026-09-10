package runtime

import (
	"fmt"

	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 本文件是策略注册表的收集层（机制重设计 M2）：从 proto descriptor 构造
// domainauth.PolicySet（domain 纯类型的唯一注入点）。proto 是策略的唯一
// 声明源；本文件持有 proto enum ↔ domain 词表的映射，映射两侧词表由
// domainauth 的常量与断言锁定（AssertSemantic 会拒绝未登记值）。

// protoAdminRole 映射 proto AdminRole enum → domain AdminRole。
// 未登记值（含 UNSPECIFIED）返回零值 + false。
func protoAdminRole(r sharedv1.AdminRole) (domainauth.AdminRole, bool) {
	switch r {
	case sharedv1.AdminRole_ADMIN_ROLE_VIEWER:
		return domainauth.AdminRoleViewer, true
	case sharedv1.AdminRole_ADMIN_ROLE_MEMBER:
		return domainauth.AdminRoleMember, true
	case sharedv1.AdminRole_ADMIN_ROLE_ADMIN:
		return domainauth.AdminRoleAdmin, true
	case sharedv1.AdminRole_ADMIN_ROLE_OWNER:
		return domainauth.AdminRoleOwner, true
	default:
		return "", false
	}
}

// protoScopeResource 映射 proto ScopeResource enum → domain ScopeResource。
func protoScopeResource(r sharedv1.ScopeResource) (domainauth.ScopeResource, bool) {
	switch r {
	case sharedv1.ScopeResource_SCOPE_RESOURCE_DATABASES:
		return domainauth.ScopeDatabases, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_USERS:
		return domainauth.ScopeUsers, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_GROUPS:
		return domainauth.ScopeGroups, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_STORAGE:
		return domainauth.ScopeStorage, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_PROJECTS:
		return domainauth.ScopeProjects, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_OAUTH_PROVIDERS:
		return domainauth.ScopeOAuthProviders, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_FUNCTIONS:
		return domainauth.ScopeFunctions, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_PAYMENTS:
		return domainauth.ScopePayments, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_ASSETS:
		return domainauth.ScopeAssets, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_SUBSCRIPTIONS:
		return domainauth.ScopeSubscriptions, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_BILLING:
		return domainauth.ScopeBilling, true
	case sharedv1.ScopeResource_SCOPE_RESOURCE_OUTBOX:
		return domainauth.ScopeOutbox, true
	default:
		return "", false
	}
}

func protoAccessLevel(l sharedv1.AccessLevel) (domainauth.AccessLevel, error) {
	switch l {
	case sharedv1.AccessLevel_ACCESS_PUBLIC:
		return domainauth.AccessPublic, nil
	case sharedv1.AccessLevel_ACCESS_END_USER:
		return domainauth.AccessEndUser, nil
	case sharedv1.AccessLevel_ACCESS_SERVER:
		return domainauth.AccessServer, nil
	case sharedv1.AccessLevel_ACCESS_PERMISSION:
		return domainauth.AccessPermission, nil
	case sharedv1.AccessLevel_ACCESS_SYSTEM:
		return domainauth.AccessSystem, nil
	default:
		return domainauth.AccessLevelUnspecified, fmt.Errorf("access level %s 未登记", l)
	}
}

// BuildMethodPolicies 从业务 proto 文件清单构造全量策略注册表。
// END_USER 面 permissions 为空时归一为 ["users"]（client 面端用户语义）。
func BuildMethodPolicies(fileDescs ...protoreflect.FileDescriptor) (*domainauth.PolicySet, error) {
	var policies []domainauth.MethodPolicy
	for _, fileDesc := range fileDescs {
		services := fileDesc.Services()
		for i := 0; i < services.Len(); i++ {
			service := services.Get(i)
			serviceName := "/" + string(service.FullName())
			serviceDefault := resolveServiceDefaultAccess(service)
			methods := service.Methods()
			for j := 0; j < methods.Len(); j++ {
				method := methods.Get(j)
				p, err := buildMethodPolicy(serviceName, method, serviceDefault)
				if err != nil {
					return nil, err
				}
				policies = append(policies, p)
			}
		}
	}
	return domainauth.NewPolicySet(policies)
}

// buildMethodPolicy 解析单个方法策略：method_auth 优先，缺省回落 service
// 默认 access（细粒度字段 admin_roles/api_key_scope/permissions 仅来自
// method_auth——服务级不携带）。
func buildMethodPolicy(serviceName string, method protoreflect.MethodDescriptor, serviceDefault sharedv1.AccessLevel) (domainauth.MethodPolicy, error) {
	access := serviceDefault
	var auth *sharedv1.MethodAuth
	if options := method.Options(); options != nil {
		if ext := proto.GetExtension(options, sharedv1.E_MethodAuth); ext != nil {
			if ma, ok := ext.(*sharedv1.MethodAuth); ok && ma != nil {
				if ma.GetAccess() != sharedv1.AccessLevel_ACCESS_LEVEL_UNSPECIFIED {
					access = ma.GetAccess()
				}
				auth = ma
			}
		}
	}
	p := domainauth.MethodPolicy{
		Method:              serviceName + "/" + string(method.Name()),
		Service:             serviceName,
		RequestHasProjectID: method.Input() != nil && method.Input().Fields().ByName("project_id") != nil,
		IsStreaming:         method.IsStreamingClient() || method.IsStreamingServer(),
	}
	var err error
	if p.Access, err = protoAccessLevel(access); err != nil {
		return p, fmt.Errorf("method %s: %w", p.Method, err)
	}
	if p.Access == domainauth.AccessLevelUnspecified {
		return p, fmt.Errorf("missing auth policy for method %s", p.Method)
	}
	if auth != nil {
		p.Permissions = append([]string{}, auth.GetPermissions()...)
	}
	// END_USER 面 permissions 为空时归一为 ["users"]（client 面端用户语义，
	// 无论策略来自方法级声明还是服务级默认）。
	if p.Access == domainauth.AccessEndUser && len(p.Permissions) == 0 {
		p.Permissions = []string{domainauth.RoleEndUserTag}
	}
	if auth == nil {
		return p, nil
	}
	for _, r := range auth.GetAdminRoles() {
		role, ok := protoAdminRole(r)
		if !ok {
			return p, fmt.Errorf("method %s: admin_roles 值 %s 未登记", p.Method, r)
		}
		p.AdminRoles = append(p.AdminRoles, role)
	}
	if scope := auth.GetApiKeyScope(); scope != nil {
		res, ok := protoScopeResource(scope.GetResource())
		if !ok {
			return p, fmt.Errorf("method %s: api_key_scope 资源 %s 未登记", p.Method, scope.GetResource())
		}
		var op domainauth.ScopeOp
		switch scope.GetOp() {
		case sharedv1.ScopeOp_SCOPE_OP_READ:
			op = domainauth.ScopeRead
		case sharedv1.ScopeOp_SCOPE_OP_WRITE:
			op = domainauth.ScopeWrite
		default:
			return p, fmt.Errorf("method %s: api_key_scope 方向 %s 未登记", p.Method, scope.GetOp())
		}
		p.Scope = &domainauth.ScopeRule{Resource: res, Op: op}
	}
	return p, nil
}
