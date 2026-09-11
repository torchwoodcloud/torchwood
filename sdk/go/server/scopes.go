package server

// API key scope 词表常量（Phase B）：由策略注册表派生（对应
// `GET /.well-known/torchwood` 下发的 api_key_scopes 段），资源清单变更时
// 重新生成。服务端匹配语义见 PolicySet.AllowsAPIKey：`*` / all 全量放行；
// 裸资源名放行该资源全部方法；`<res>.read`/`<res>.write` 仅对应方向。
// 注意：scope 只对 SERVER 面方法生效，PERMISSION 面（API Key 管理等）
// 即使 `*` 也一律拒绝。
const (
	// ScopeWildcard 是全量通配 scope；ScopeAll 为其等价别名。
	ScopeWildcard = "*"
	// ScopeAll 是全量通配 scope（等价 ScopeWildcard）。
	ScopeAll = "all"

	// ScopeDatabases 文档/库表面（DDL 为 delegated 档，写 = databases.write）。
	ScopeDatabases      = "databases"
	ScopeDatabasesRead  = "databases.read"
	ScopeDatabasesWrite = "databases.write"

	// ScopeUsers 用户管理面（server 面用户 CRUD/会话/令牌）。
	ScopeUsers      = "users"
	ScopeUsersRead  = "users.read"
	ScopeUsersWrite = "users.write"

	// ScopeGroups 组管理面。
	ScopeGroups      = "groups"
	ScopeGroupsRead  = "groups.read"
	ScopeGroupsWrite = "groups.write"

	// ScopeStorage 存储桶/文件面。
	ScopeStorage      = "storage"
	ScopeStorageRead  = "storage.read"
	ScopeStorageWrite = "storage.write"

	// ScopeProjects 项目面（建删项目为 PERMISSION 面，key 不可用；
	// key 通道仅 projects.read / projects.write 的项目读写）。
	ScopeProjects      = "projects"
	ScopeProjectsRead  = "projects.read"
	ScopeProjectsWrite = "projects.write"

	// ScopeOAuthProviders OAuth 提供方配置面。
	ScopeOAuthProviders      = "oauthproviders"
	ScopeOAuthProvidersRead  = "oauthproviders.read"
	ScopeOAuthProvidersWrite = "oauthproviders.write"

	// ScopeFunctions Functions 部署与执行面。
	ScopeFunctions      = "functions"
	ScopeFunctionsRead  = "functions.read"
	ScopeFunctionsWrite = "functions.write"

	// ScopePayments 支付/订单面。
	ScopePayments      = "payments"
	ScopePaymentsRead  = "payments.read"
	ScopePaymentsWrite = "payments.write"

	// ScopeAssets 资产/账本面（授信类动作为 delegated 档）。
	ScopeAssets      = "assets"
	ScopeAssetsRead  = "assets.read"
	ScopeAssetsWrite = "assets.write"

	// ScopeSubscriptions 订阅面。
	ScopeSubscriptions      = "subscriptions"
	ScopeSubscriptionsRead  = "subscriptions.read"
	ScopeSubscriptionsWrite = "subscriptions.write"

	// ScopeBilling 账单/用量面（只读）。
	ScopeBilling      = "billing"
	ScopeBillingRead  = "billing.read"
	ScopeBillingWrite = "billing.write"

	// ScopeOutbox 事件死信面（重放 = outbox.write，delegated 档）。
	ScopeOutbox      = "outbox"
	ScopeOutboxRead  = "outbox.read"
	ScopeOutboxWrite = "outbox.write"

	// ScopeAuditLogs 审计日志查询面（当前仅只读方法；write 形态由资源
	// 词表全集派生预留，尚无对应 RPC）。
	ScopeAuditLogs      = "audit_logs"
	ScopeAuditLogsRead  = "audit_logs.read"
	ScopeAuditLogsWrite = "audit_logs.write"
)
