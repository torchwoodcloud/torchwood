package server

import (
	"testing"
)

// 本测试是 scope 词表常量的契约锁（SDK 不 import internal，无法直连策略
// 注册表）：scopeScopesContract 内嵌生成期从 domainauth.AllScopeResources
// 拷贝的期望清单；internal/runtime/sdk_scopes_contract_test.go 从另一侧
// 断言本文件 const 块与真实词表对齐（双层锁定，资源清单变更时两处同时更新
// ——先重新生成常量，再更新本清单）。

// scopeVocabularyContract 是生成期拷贝的合法 scope 全集
// （= 全部资源 {资源名, 资源名.read, 资源名.write} ∪ {*, all}）。
var scopeVocabularyContract = []string{
	"*", "all",
	"databases", "databases.read", "databases.write",
	"users", "users.read", "users.write",
	"groups", "groups.read", "groups.write",
	"storage", "storage.read", "storage.write",
	"projects", "projects.read", "projects.write",
	"oauthproviders", "oauthproviders.read", "oauthproviders.write",
	"functions", "functions.read", "functions.write",
	"payments", "payments.read", "payments.write",
	"assets", "assets.read", "assets.write",
	"subscriptions", "subscriptions.read", "subscriptions.write",
	"billing", "billing.read", "billing.write",
	"outbox", "outbox.read", "outbox.write",
	"audit_logs", "audit_logs.read", "audit_logs.write",
	"analytics", "analytics.read", "analytics.write",
}

func TestScopeConstantsMatchContract(t *testing.T) {
	want := map[string]bool{}
	for _, s := range scopeVocabularyContract {
		want[s] = true
	}

	got := map[string]bool{
		ScopeWildcard: true, ScopeAll: true,
		ScopeDatabases: true, ScopeDatabasesRead: true, ScopeDatabasesWrite: true,
		ScopeUsers: true, ScopeUsersRead: true, ScopeUsersWrite: true,
		ScopeGroups: true, ScopeGroupsRead: true, ScopeGroupsWrite: true,
		ScopeStorage: true, ScopeStorageRead: true, ScopeStorageWrite: true,
		ScopeProjects: true, ScopeProjectsRead: true, ScopeProjectsWrite: true,
		ScopeOAuthProviders: true, ScopeOAuthProvidersRead: true, ScopeOAuthProvidersWrite: true,
		ScopeFunctions: true, ScopeFunctionsRead: true, ScopeFunctionsWrite: true,
		ScopePayments: true, ScopePaymentsRead: true, ScopePaymentsWrite: true,
		ScopeAssets: true, ScopeAssetsRead: true, ScopeAssetsWrite: true,
		ScopeSubscriptions: true, ScopeSubscriptionsRead: true, ScopeSubscriptionsWrite: true,
		ScopeBilling: true, ScopeBillingRead: true, ScopeBillingWrite: true,
		ScopeOutbox: true, ScopeOutboxRead: true, ScopeOutboxWrite: true,
		ScopeAuditLogs: true, ScopeAuditLogsRead: true, ScopeAuditLogsWrite: true,
		ScopeAnalytics: true, ScopeAnalyticsRead: true, ScopeAnalyticsWrite: true,
	}

	if len(got) != len(want) {
		t.Fatalf("scope 常量数量与契约不符：got %d, want %d", len(got), len(want))
	}
	for s := range got {
		if !want[s] {
			t.Errorf("scope 常量 %q 不在契约清单（词表演进后未同步 scopes.go / scopes_test.go）", s)
		}
	}
	for s := range want {
		if !got[s] {
			t.Errorf("契约 scope %q 缺少对应常量", s)
		}
	}
}
