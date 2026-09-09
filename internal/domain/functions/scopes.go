package functions

import (
	"fmt"
	"sort"
	"strings"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// DeclaredScopes 词表（P0 执行身份，设计 §1「危险资源排除」）：函数执行
// principal 只开放数据面资源；functions/projects/billing/outbox/oauthproviders
// 等平台/编排面资源一律拒绝（deny 语义经 allow-list 实现——词表外的 resource
// 全部拒绝，含自我复制的 functions:* 递归放大面）。
//
// 存储形态："<resource>:<op>"（如 assets:write）；运行时经
// DeclaredScopePermission 投影为 API key 同款权限串（"<res>.<op>"，如
// assets.write）供 PolicySet.AllowsAPIKeyTargets 原样求值——scope 语义与
// API key 完全一致，词表值域是 domainauth.ScopeResource 的子集。
var declaredScopeResources = map[domainauth.ScopeResource]struct{}{
	domainauth.ScopeAssets:        {},
	domainauth.ScopeDatabases:     {},
	domainauth.ScopeUsers:         {},
	domainauth.ScopeGroups:        {},
	domainauth.ScopeStorage:       {},
	domainauth.ScopeSubscriptions: {},
	domainauth.ScopePayments:      {},
}

// DeclaredScopeOps 是 scope 的读写方向（与 domainauth.ScopeOp 词表一致）。
var declaredScopeOps = map[domainauth.ScopeOp]struct{}{
	domainauth.ScopeRead:  {},
	domainauth.ScopeWrite: {},
}

// ParseDeclaredScope 解析单个 declared scope 项（"<resource>:<op>" 严格形态；
// 禁止 API key 的实例限定/通配符/裸资源形态——执行身份的最小特权不做放大）。
func ParseDeclaredScope(s string) (resource domainauth.ScopeResource, op domainauth.ScopeOp, err error) {
	res, opStr, found := strings.Cut(s, ":")
	if !found || res == "" || opStr == "" {
		return "", "", fmt.Errorf("declared scope %q must match <resource>:<op>", s)
	}
	resource = domainauth.ScopeResource(res)
	if _, ok := declaredScopeResources[resource]; !ok {
		return "", "", fmt.Errorf("declared scope resource %q is not allowed", res)
	}
	op = domainauth.ScopeOp(opStr)
	if _, ok := declaredScopeOps[op]; !ok {
		return "", "", fmt.Errorf("declared scope op %q is not allowed", opStr)
	}
	return resource, op, nil
}

// NormalizeDeclaredScopes 校验并规范化 scope 集合：逐项 ParseDeclaredScope、
// 自动去重、按字典序稳定排序（全量替换语义下的规范存储形态）。空集合法
// （= 无平台访问权限，fail-closed 默认）。
func NormalizeDeclaredScopes(scopes []string) ([]string, error) {
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		trimmed := strings.TrimSpace(s)
		if _, _, err := ParseDeclaredScope(trimmed); err != nil {
			return nil, err
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	sort.Strings(out)
	return out, nil
}

// DeclaredScopePermission 把 declared scope（"assets:write"）投影为 API key
// 同款权限串（"assets.write"），供 principal.Permissions 复用既有 scope 门。
// 非法项返回 false（构造 principal 时跳过——存储侧已经 Normalize 兜底）。
func DeclaredScopePermission(scope string) (string, bool) {
	res, op, err := ParseDeclaredScope(scope)
	if err != nil {
		return "", false
	}
	return string(res) + "." + string(op), true
}

// DeclaredScopePermissions 是 DeclaredScopePermission 的集合投影（去重保序）。
func DeclaredScopePermissions(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	seen := map[string]struct{}{}
	for _, s := range scopes {
		p, ok := DeclaredScopePermission(s)
		if !ok {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
