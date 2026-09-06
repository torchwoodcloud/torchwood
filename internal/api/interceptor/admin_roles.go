package interceptor

// adminRoleMethodRules 登记 Server API 写方法（及受限读方法）对应的允许
// 角色。策略单一声明源已迁 proto method_auth（机制重设计 M1/M2），本表是
// A2 拦截器换源 PolicySet 前的过渡执行表，与 proto 声明由
// AssertAdminRoleWriteCoverage + AssertAPIKeyScopeCoverage 启动断言锁定一致。
//
// 角色模型（对齐 Console useAdminRole）：viewer 只读；member 可写业务资源；
// owner/admin（平台 admin）不受限。档位语义（决策 v8）：
//
//	业务写（member+）：用户六写方法、文档 CRUD、storage/groups/plans/catalog；
//	委托自动化（owner/admin + key scope）：DDL、Functions、OAuth 提供方、
//	  退款/履约、资产五动词、死信重放、UpdateProject；
//	平台专属（PERMISSION，不在本表）：项目建删、API Key 管理（key 禁入）。
var adminRoleMethodRules = map[string][]string{
	// UsersService（决策 v8：六写方法归一业务写档——member+users.write；
	// 接管信任边界收敛到 scope 授予环节，key 仅 owner/admin 可建）
	"/torchwood.server.v1.UsersService/CreateUser":         {"member", "owner", "admin"},
	"/torchwood.server.v1.UsersService/UpdateUser":         {"member", "owner", "admin"},
	"/torchwood.server.v1.UsersService/UpdateUserPassword": {"member", "owner", "admin"},
	"/torchwood.server.v1.UsersService/DeleteUser":         {"member", "owner", "admin"},
	"/torchwood.server.v1.UsersService/DeleteUserSession":  {"member", "owner", "admin"},
	"/torchwood.server.v1.UsersService/CreateUserToken":    {"member", "owner", "admin"},
	// DatabasesService（schema DDL 写方法，仅 owner/admin）
	"/torchwood.server.v1.DatabasesService/CreateDatabase":   {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/DeleteDatabase":   {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/CreateCollection": {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/UpdateCollection": {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/DeleteCollection": {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/CreateAttribute":  {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/DeleteAttribute":  {"owner", "admin"},
	// B4 schema 演进生命周期（§4.6）：同 schema DDL 写面，仅 owner/admin。
	"/torchwood.server.v1.DatabasesService/RestoreAttribute": {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/RetireAttribute":  {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/MigrateAttribute": {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/CreateIndex":      {"owner", "admin"},
	"/torchwood.server.v1.DatabasesService/DeleteIndex":      {"owner", "admin"},
	// DatabasesService 文档 CRUD 写方法（业务写，member 可做）
	"/torchwood.server.v1.DatabasesService/CreateDocument":      {"member", "owner", "admin"},
	"/torchwood.server.v1.DatabasesService/UpdateDocument":      {"member", "owner", "admin"},
	"/torchwood.server.v1.DatabasesService/UpsertDocument":      {"member", "owner", "admin"},
	"/torchwood.server.v1.DatabasesService/DeleteDocument":      {"member", "owner", "admin"},
	"/torchwood.server.v1.DatabasesService/BulkUpdateDocuments": {"member", "owner", "admin"},
	"/torchwood.server.v1.DatabasesService/BulkDeleteDocuments": {"member", "owner", "admin"},
	"/torchwood.server.v1.DatabasesService/ExecuteTransactions": {"member", "owner", "admin"},
	// FunctionsService 全部写方法（对照 proto/server/v1/functions.proto RPC
	// 清单逐一登记；GetVariables 返回掩码值安全可放行，其余读方法 viewer 可读）
	"/torchwood.server.v1.FunctionsService/CreateFunction":   {"owner", "admin"},
	"/torchwood.server.v1.FunctionsService/UpdateFunction":   {"owner", "admin"},
	"/torchwood.server.v1.FunctionsService/DeleteFunction":   {"owner", "admin"},
	"/torchwood.server.v1.FunctionsService/CreateDeployment": {"owner", "admin"},
	"/torchwood.server.v1.FunctionsService/DeleteDeployment": {"owner", "admin"},
	"/torchwood.server.v1.FunctionsService/SetVariables":     {"owner", "admin"},
	"/torchwood.server.v1.FunctionsService/CreateExecution":  {"owner", "admin"},
	// OAuthProvidersService（仅 owner/admin）
	"/torchwood.server.v1.OAuthProvidersService/UpsertOAuthProvider": {"owner", "admin"},
	"/torchwood.server.v1.OAuthProvidersService/DeleteOAuthProvider": {"owner", "admin"},
	// StorageService（业务写，member 可做）
	"/torchwood.server.v1.StorageService/CreateBucket":    {"member", "owner", "admin"},
	"/torchwood.server.v1.StorageService/UpdateBucket":    {"member", "owner", "admin"},
	"/torchwood.server.v1.StorageService/DeleteBucket":    {"member", "owner", "admin"},
	"/torchwood.server.v1.StorageService/CreateFile":      {"member", "owner", "admin"},
	"/torchwood.server.v1.StorageService/DeleteFile":      {"member", "owner", "admin"},
	"/torchwood.server.v1.StorageService/UpdateFile":      {"member", "owner", "admin"},
	"/torchwood.server.v1.StorageService/CreateFileToken": {"member", "owner", "admin"},
	// GroupsService（业务写，member 可做；Client Groups API 复用同一 use-case，
	// 不套 RequireServerWriteActor）
	"/torchwood.server.v1.GroupsService/CreateGroup":            {"member", "owner", "admin"},
	"/torchwood.server.v1.GroupsService/DeleteGroup":            {"member", "owner", "admin"},
	"/torchwood.server.v1.GroupsService/CreateMembership":       {"member", "owner", "admin"},
	"/torchwood.server.v1.GroupsService/UpdateMembership":       {"member", "owner", "admin"},
	"/torchwood.server.v1.GroupsService/UpdateMembershipStatus": {"member", "owner", "admin"},
	"/torchwood.server.v1.GroupsService/DeleteMembership":       {"member", "owner", "admin"},
	"/torchwood.server.v1.GroupsService/UpdateGroupPrefs":       {"member", "owner", "admin"},
	// PaymentsService（金额敏感：退款 / 人工履约仅 owner/admin；
	// 与 apikeys 同级——直接资金操作，viewer/member 不可触发）
	"/torchwood.server.v1.PaymentsService/Refund":        {"owner", "admin"},
	"/torchwood.server.v1.PaymentsService/ManualFulfill": {"owner", "admin"},
	// AssetsService：目录 CRUD 为业务写（member 可做）；五动词与对账为资产
	// 变动（仅 owner/admin，与退款同级）。
	"/torchwood.server.v1.AssetsService/CreateAssetDef": {"member", "owner", "admin"},
	"/torchwood.server.v1.AssetsService/UpdateAssetDef": {"member", "owner", "admin"},
	"/torchwood.server.v1.AssetsService/DeleteAssetDef": {"member", "owner", "admin"},
	"/torchwood.server.v1.AssetsService/Grant":          {"owner", "admin"},
	"/torchwood.server.v1.AssetsService/Consume":        {"owner", "admin"},
	"/torchwood.server.v1.AssetsService/Transfer":       {"owner", "admin"},
	"/torchwood.server.v1.AssetsService/Mutate":         {"owner", "admin"},
	"/torchwood.server.v1.AssetsService/Expire":         {"owner", "admin"},
	"/torchwood.server.v1.AssetsService/Reconcile":      {"owner", "admin"},
	// SubscriptionsService：计划 CRUD 为业务写（member 可做）；强制 Cancel/Expire
	// 为资金相关（仅 owner/admin）。
	"/torchwood.server.v1.SubscriptionsService/CreatePlan":         {"member", "owner", "admin"},
	"/torchwood.server.v1.SubscriptionsService/UpdatePlan":         {"member", "owner", "admin"},
	"/torchwood.server.v1.SubscriptionsService/DeletePlan":         {"member", "owner", "admin"},
	"/torchwood.server.v1.SubscriptionsService/CancelSubscription": {"owner", "admin"},
	"/torchwood.server.v1.SubscriptionsService/ExpireSubscription": {"owner", "admin"},
	// ProjectsService（决策 v8：创建/删除挪 PERMISSION 平台专属——不在本表；
	// 更新是业务写）
	"/torchwood.server.v1.ProjectsService/UpdateProject": {"member", "owner", "admin"},
	// OutboxService（决策 v8：死信面委托平台档——payload 含文档数据，
	// 读面也限 owner/admin，viewer 不再可枚举）
	"/torchwood.server.v1.OutboxService/ListDeadLetters":  {"owner", "admin"},
	"/torchwood.server.v1.OutboxService/ReplayDeadLetter": {"owner", "admin"},
}
