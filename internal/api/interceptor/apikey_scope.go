package interceptor

import (
	"fmt"
	"sort"

	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
)

// Thin forwarding layer (Deprecated): 策略表单一事实来源已迁至 internal/domain/auth/scope.go（J4-5）。
// 本文件保留薄转发以兼容既有 import 路径（interceptor.APIKeyScopeAllowed 等），
// 标记为 Deprecated，新代码应直接 import domainauth。

// Deprecated: Use domainauth.StorageServiceCreateFile.
const StorageServiceCreateFile = domainauth.StorageServiceCreateFile

// Deprecated: Use domainauth.StorageServiceGetFile.
const StorageServiceGetFile = domainauth.StorageServiceGetFile

// apiKeyScopeRule 保留私有类型定义以兼容同包测试（如 admin_roles_test.go
// 中构造 map[string]apiKeyScopeRule 的字面量）；单一事实来源以 domainauth 为准，
// 本地 map 由 domain 重建以保持同步。
type apiKeyScopeRule struct {
	resource string
	op       string
}

// apiKeyScopeRules 由 domainauth.APIKeyScopeRules 重建，保持与 domain 单一事实来源同步
// （fail-closed 断言仍以 domain 为准）。
var apiKeyScopeRules = func() map[string]apiKeyScopeRule {
	m := domainauth.APIKeyScopeRules()
	out := make(map[string]apiKeyScopeRule, len(m))
	for k, v := range m {
		out[k] = apiKeyScopeRule{resource: v.Resource, op: v.Op}
	}
	return out
}()

// APIKeyScopeRule re-export (Deprecated: Use domainauth.APIKeyScopeRule).
type APIKeyScopeRule = domainauth.APIKeyScopeRule

// Deprecated: Use domainauth.APIKeyScopeAllowed.
func APIKeyScopeAllowed(fullMethod string, scopes []string) bool {
	return domainauth.APIKeyScopeAllowed(fullMethod, scopes)
}

// Deprecated: Use domainauth.ValidAPIKeyScope.
func ValidAPIKeyScope(s string) bool {
	return domainauth.ValidAPIKeyScope(s)
}

// Deprecated: Use domainauth.IsAPIKeysServiceMethod.
func IsAPIKeysServiceMethod(fullMethod string) bool {
	return domainauth.IsAPIKeysServiceMethod(fullMethod)
}

// Deprecated: Use domainauth.APIKeyScopeRules.
func APIKeyScopeRules() map[string]APIKeyScopeRule {
	return domainauth.APIKeyScopeRules()
}

// Deprecated: Use domainauth.AssertAPIKeyScopeCoverage.
func AssertAPIKeyScopeCoverage(apiKeyMethods []string) {
	domainauth.AssertAPIKeyScopeCoverage(apiKeyMethods)
}

// adminRoleWriteCoverageDiff 比较 scope 写方法集合与角色表，返回：
//   - missing：scope 表声明 op=="write" 但角色表未登记的方法；
//   - extra：角色表登记了读方法（op=="read"）或 scope 表不存在的方法。
//
// 抽成纯函数便于单测构造缺失/多余场景（AssertAdminRoleWriteCoverage 直接调用）。
func adminRoleWriteCoverageDiff(scopeRules map[string]apiKeyScopeRule, roleRules map[string][]string) (missing, extra []string) {
	for m, rule := range scopeRules {
		if rule.op != "write" {
			continue
		}
		if _, ok := roleRules[m]; !ok {
			missing = append(missing, m)
		}
	}
	for m := range roleRules {
		rule, ok := scopeRules[m]
		if !ok {
			extra = append(extra, m)
			continue
		}
		if rule.op != "write" {
			extra = append(extra, m)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// AssertAdminRoleWriteCoverage 断言 adminRoleMethodRules 与 apiKeyScopeRules
// 的写方法集合完全一致（Round3 H1-1，fail-closed）。
// 保持原位（依赖 adminRoleMethodRules 定于 admin_roles.go），但 scope 来源已与 domain 同步。
func AssertAdminRoleWriteCoverage() {
	missing, extra := adminRoleWriteCoverageDiff(apiKeyScopeRules, adminRoleMethodRules)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	panic(fmt.Sprintf("adminRoleMethodRules 与 apiKeyScopeRules 写方法集合不一致 (fail-closed): "+
		"写方法未登记角色=%v; 角色表多余/指向读方法=%v", missing, extra))
}
