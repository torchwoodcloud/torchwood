package auth

import (
	"fmt"
	"strings"

	"github.com/lynx-go/grpcapi/policy"
)

// 本文件是五档访问语义与 grpcapi v3 requirement 树的双向映射（执行侧迁移，
// grpcapi GENERALIZATION §11.4）：Expand 把旧五档注解展开为库 RequirementDecider
// 可直接求值的规范树（legacyProvider 与测试装配共用）；View 把规范树识别回
// MethodPolicy 视图（断言/矩阵/文档/golden 消费）。树是唯一策略载体——本包
// 不再维护第二棵投影树，全仓库也不存在第二个授权执行点。
//
// 规范树形（五档）：
//
//	PUBLIC     {anonymous}
//	END_USER   require[principal_kind(user)]
//	PERMISSION require[permission(perms, match=any)]
//	SERVER     any_of[ [admin 门], [scope(res:op)], [resource(res)]? ]
//
// SERVER 档细节（与旧 interceptor 手工判定逐语义对齐）：
//   - admin 门：声明 admin_roles 时 = admin_role(roles...)；未声明（不限角色，
//     viewer 可调）= principal_kind(admin)。admin 会话在 gRPC 链的
//     X-Torchwood-Project / ValidateAdminProjectAccess 业务校验仍在 torchwood
//     authn 拦截器原位执行。
//   - scope 门：操作级判定（`*`/`all`/裸资源/res:op 命中即过）。
//   - resource 门（可选）：实例寻址判定，仅当资源可实例寻址且请求携带寻址
//     字段时展开；admin 门不受实例门约束（组间 OR）。
//   - 组间 OR 复现旧的逐 token 合取语义：单 token 只需命中任一门——操作级
//     token（databases.read）过 scope 门、实例 token（databases:blog）过
//     resource 门、`*`/裸资源两者皆过。与 composite_gate 的组内 AND 范式
// （要求 key 同时持有操作级 + 实例级 token）不同：AND 会让 `databases.read`
//     这类操作级 token 在实例寻址方法上被拒，违反「scope 四形态行为不变」
//     验收；差异记录见迁移报告。
//   - PERMISSION 门 match=any：旧执行语义为 HasAnyRole（任一命中）。
const (
	kindPrincipal  = "principal_kind"
	kindAdminRole  = "admin_role"
	kindPermission = "permission"
	kindScope      = "scope"
	kindResource   = "resource"
)

// InstanceFields 返回资源族的实例寻址请求字段（按优先级）；非可寻址资源
// 返回 nil。与旧 interceptor.apiKeyScopeTargets 的 getter 优先级一致
// （databases: database_id→id；storage: bucket_id→id）。
func InstanceFields(resource string) []string {
	switch ScopeResource(resource) {
	case ScopeDatabases:
		return []string{"database_id", "id"}
	case ScopeStorage:
		return []string{"bucket_id", "id"}
	default:
		return nil
	}
}

// InstanceExtractorFields 是注册进库 resource kind 的 ID 提取器字段序：
// database_id 与 bucket_id 不会出现在同一请求（资源族互斥），id 兜底
// （CreateDatabase/CreateBucket 等以 id 寻址被创建资源）。
var InstanceExtractorFields = []string{"database_id", "bucket_id", "id"}

// TargetedRequest 携带传输层提取的实例目标，供无 proto 请求体的 HTTP 面
// （存储 multipart 上传/下载等）递给库 resource 门求值；gRPC 面走 proto
// 字段提取，不经此类型。ID 为空等价于不寻址实例（哨兵语义）。
type TargetedRequest struct {
	ID string
}

// TreeSpec 是五档展开的输入（legacy 注解字段的直接投影）。
type TreeSpec struct {
	Access      AccessLevel
	Permissions []string
	AdminRoles  []string
	Scope       *ScopeRule
	// InstanceAddressed 报告请求消息是否携带资源实例寻址字段（调用方从
	// descriptor 判定）；false 时 SERVER 档省略 resource 门（全集型方法对
	// 实例限定 scope 恒不匹配，与旧语义一致）。
	InstanceAddressed bool
}

// Expand 展开为规范 requirement 树；非法组合返回错误（Build 期 fail-closed）。
func (s TreeSpec) Expand() (policy.Policy, error) {
	switch s.Access {
	case AccessPublic:
		return policy.Policy{Anonymous: true}, nil
	case AccessEndUser:
		return policy.Policy{Require: []policy.Requirement{{Kind: kindPrincipal, Values: []string{PrincipalKindUser}}}}, nil
	case AccessPermission:
		if len(s.Permissions) == 0 {
			return policy.Policy{}, fmt.Errorf("PERMISSION access must declare explicit permissions")
		}
		return policy.Policy{Require: []policy.Requirement{{
			Kind:   kindPermission,
			Match:  policy.MatchAny,
			Values: append([]string(nil), s.Permissions...),
		}}}, nil
	case AccessServer:
		if s.Scope == nil {
			return policy.Policy{}, fmt.Errorf("SERVER access must declare api_key_scope (use PERMISSION if not exposed to API keys)")
		}
		groups := [][]policy.Requirement{s.adminGate(), s.scopeGate()}
		if s.InstanceAddressed {
			groups = append(groups, []policy.Requirement{{Kind: kindResource, Values: []string{s.Scope.Resource}}})
		}
		return policy.Policy{AnyOf: groups}, nil
	case AccessSystem:
		return policy.Policy{}, fmt.Errorf("SYSTEM access has no external contract yet and is forbidden")
	default:
		return policy.Policy{}, fmt.Errorf("access is not declared")
	}
}

func (s TreeSpec) adminGate() []policy.Requirement {
	if len(s.AdminRoles) > 0 {
		return []policy.Requirement{{Kind: kindAdminRole, Values: append([]string(nil), s.AdminRoles...)}}
	}
	// 未声明角色 = 不限角色，任何 admin 会话主体可入。
	return []policy.Requirement{{Kind: kindPrincipal, Values: []string{PrincipalKindAdmin}}}
}

func (s TreeSpec) scopeGate() []policy.Requirement {
	return []policy.Requirement{{Kind: kindScope, Values: []string{s.Scope.Resource + ":" + string(s.Scope.Op)}}}
}

// View 把规范树识别回 MethodPolicy（断言/矩阵/文档/golden 的消费视图）。
// 非规范树形返回错误——legacyProvider 只产出五档规范形，识别失败即策略源
// 漂移，fail-closed。组序固定为展开序（admin, scope, [resource]）。
func View(op policy.Operation) (MethodPolicy, error) {
	mp := MethodPolicy{
		Method:        op.ID,
		Service:       "/" + op.Service,
		RequestFields: append([]string(nil), op.RequestFields...),
		IsStreaming:   op.Streaming,
	}
	p := op.Policy
	switch {
	case p.Anonymous && len(p.Require) == 0 && len(p.AnyOf) == 0 && len(p.Forbid) == 0:
		mp.Access = AccessPublic
	case len(p.Require) == 1 && len(p.AnyOf) == 0 && len(p.Forbid) == 0:
		r := p.Require[0]
		switch {
		case r.Kind == kindPrincipal && r.Match == policy.MatchDefault && len(r.Values) == 1 && r.Values[0] == PrincipalKindUser:
			mp.Access = AccessEndUser
			// 旧投影归一：END_USER 恒补 ["users"]（assertClientValueDomain 锁定）。
			mp.Permissions = []string{RoleEndUserTag}
		case r.Kind == kindPermission && len(r.Values) > 0:
			mp.Access = AccessPermission
			mp.Permissions = append([]string(nil), r.Values...)
		default:
			return mp, fmt.Errorf("non-canonical policy tree (require: %s)", formatRequirement(r))
		}
	case len(p.Require) == 0 && len(p.AnyOf) >= 2 && len(p.Forbid) == 0:
		mp.Access = AccessServer
		if err := viewAdminGate(p.AnyOf[0], &mp); err != nil {
			return mp, err
		}
		scope := p.AnyOf[1]
		if len(scope) != 1 || scope[0].Kind != kindScope || len(scope[0].Values) != 1 {
			return mp, fmt.Errorf("non-canonical policy tree (scope gate)")
		}
		res, opName, ok := strings.Cut(scope[0].Values[0], ":")
		if !ok || res == "" || opName == "" {
			return mp, fmt.Errorf("non-canonical scope value %q", scope[0].Values[0])
		}
		mp.Scope = &ScopeRule{Resource: res, Op: ScopeOp(opName)}
		if len(p.AnyOf) == 3 {
			rg := p.AnyOf[2]
			if len(rg) != 1 || rg[0].Kind != kindResource || len(rg[0].Values) != 1 || rg[0].Values[0] != res {
				return mp, fmt.Errorf("non-canonical policy tree (resource gate)")
			}
		} else if len(p.AnyOf) > 3 {
			return mp, fmt.Errorf("non-canonical policy tree (extra any_of groups)")
		}
	default:
		return mp, fmt.Errorf("non-canonical policy tree (anonymous/require/any_of combination)")
	}
	return mp, nil
}

func viewAdminGate(group []policy.Requirement, mp *MethodPolicy) error {
	if len(group) != 1 {
		return fmt.Errorf("non-canonical policy tree (admin gate)")
	}
	r := group[0]
	switch {
	case r.Kind == kindAdminRole && len(r.Values) > 0:
		mp.AdminRoles = append([]string(nil), r.Values...)
	case r.Kind == kindPrincipal && len(r.Values) == 1 && r.Values[0] == PrincipalKindAdmin:
		// 未声明角色（nil = 不限）。
	default:
		return fmt.Errorf("non-canonical policy tree (admin gate: %s)", formatRequirement(r))
	}
	return nil
}

func formatRequirement(r policy.Requirement) string {
	return strings.TrimSpace(r.Kind + " " + strings.Join(r.Values, ","))
}
