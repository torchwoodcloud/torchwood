package interceptor

// adminRoleMethodRules 登记 Server API 全部写方法对应的允许角色（安全评审
// M7 / Round3 H1）：受限 admin（viewer）会话一律拒绝；member 可写业务资源；
// 平台敏感写（API Key 管理、用户接管面、Databases DDL、Functions、OAuth
// 提供方、项目创建/删除）仅 owner/admin。API key 与端用户凭证不在此表处理
// （分别走 apiKeyMethods scope 门禁与 use-case 层 RequireServerWriteActor
// /RequirePlatformAdmin 守门）。
//
// 角色模型对齐 Console useAdminRole：viewer 只读（仅 List/Get/Count）；
// member 可写业务资源；owner/admin（平台 admin）不受限。
//
// 完整性由 AssertAdminRoleWriteCoverage 在启动期 fail-closed 断言：
// apiKeyScopeRules 中每个 op=="write" 的方法都必须登记在本表，反之本表
// 不得出现读方法或未映射方法（见 apikey_scope.go）。
var adminRoleMethodRules = map[string][]string{
	// APIKeysService（仅 owner/admin：key 是平台级凭据，删除等同接管）
	"/torchwood.server.v1.APIKeysService/CreateAPIKey": {"owner", "admin"},
	"/torchwood.server.v1.APIKeysService/DeleteAPIKey": {"owner", "admin"},
	// UsersService
	"/torchwood.server.v1.UsersService/CreateUser":         {"member", "owner", "admin"},
	"/torchwood.server.v1.UsersService/UpdateUser":         {"owner", "admin"},
	"/torchwood.server.v1.UsersService/UpdateUserPassword": {"owner", "admin"},
	"/torchwood.server.v1.UsersService/DeleteUser":         {"owner", "admin"},
	"/torchwood.server.v1.UsersService/DeleteUserSession":  {"owner", "admin"},
	"/torchwood.server.v1.UsersService/CreateUserToken":    {"owner", "admin"},
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
	// ProjectsService（创建/删除是平台级资源，仅 owner/admin——与 use-case
	// CreateProject/DeleteProject 平台 admin 守卫一致；更新是业务写，仅收 viewer）
	"/torchwood.server.v1.ProjectsService/CreateProject": {"owner", "admin"},
	"/torchwood.server.v1.ProjectsService/UpdateProject": {"member", "owner", "admin"},
	"/torchwood.server.v1.ProjectsService/DeleteProject": {"owner", "admin"},
	// OutboxService（死信重放是平台运维敏感写，仅 owner/admin）
	"/torchwood.server.v1.OutboxService/ReplayDeadLetter": {"owner", "admin"},
}
