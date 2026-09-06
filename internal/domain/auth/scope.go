package auth

import (
	"fmt"
	"sort"
	"strings"
)

// StorageServiceCreateFile is the gRPC method used for HTTP storage scope checks.
const StorageServiceCreateFile = "/torchwood.server.v1.StorageService/CreateFile"

// StorageServiceGetFile is the gRPC method used for HTTP storage read scope checks.
const StorageServiceGetFile = "/torchwood.server.v1.StorageService/GetFile"

// apiKeyScopeRule 是单个 gRPC 方法对应的 scope 资源名与读写方向（B2）。
type apiKeyScopeRule struct {
	resource string // scope 资源名（databases/users/groups/storage/projects/oauthproviders/apikeys/functions）
	op       string // "read" 或 "write"
}

// apiKeyScopeRules 显式映射全部 ACCESS_SERVER 服务的方法（Health 是
// ACCESS_PUBLIC，不映射）。读方法 = List/Get/Count 类，其余一律 write。
// 新增 ACCESS_SERVER 方法必须在此登记，否则 APIKeyScopeAllowed 对其 fail-closed；
// 一致性由 AssertAPIKeyScopeCoverage 在启动期校验（R10-P1-5）。
var apiKeyScopeRules = map[string]apiKeyScopeRule{
	// DatabasesService
	"/torchwood.server.v1.DatabasesService/CreateDatabase":   {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/ListDatabases":    {"databases", "read"},
	"/torchwood.server.v1.DatabasesService/GetDatabase":      {"databases", "read"},
	"/torchwood.server.v1.DatabasesService/DeleteDatabase":   {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/CreateCollection": {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/ListCollections":  {"databases", "read"},
	"/torchwood.server.v1.DatabasesService/GetCollection":    {"databases", "read"},
	"/torchwood.server.v1.DatabasesService/DeleteCollection": {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/UpdateCollection": {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/CreateAttribute":  {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/DeleteAttribute":  {"databases", "write"},
	// B4 schema 演进生命周期（§4.6）：删列两段/回滚/copy 迁移——DDL 写语义。
	"/torchwood.server.v1.DatabasesService/RestoreAttribute":    {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/RetireAttribute":     {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/MigrateAttribute":    {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/CreateIndex":         {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/DeleteIndex":         {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/CreateDocument":      {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/ListDocuments":       {"databases", "read"},
	"/torchwood.server.v1.DatabasesService/GetDocument":         {"databases", "read"},
	"/torchwood.server.v1.DatabasesService/UpdateDocument":      {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/UpsertDocument":      {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/DeleteDocument":      {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/CountDocuments":      {"databases", "read"},
	"/torchwood.server.v1.DatabasesService/AggregateDocuments":  {"databases", "read"},
	"/torchwood.server.v1.DatabasesService/BulkUpdateDocuments": {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/BulkDeleteDocuments": {"databases", "write"},
	"/torchwood.server.v1.DatabasesService/ExecuteTransactions": {"databases", "write"},
	// ListChanges（阶段④ §4.5 补偿 API）：读语义——事件流按请求者可见性过滤。
	"/torchwood.server.v1.DatabasesService/ListChanges": {"databases", "read"},
	// ExportCollectionSchema（B10 §4.1/§10.1）：读语义——catalog 契约导出。
	"/torchwood.server.v1.DatabasesService/ExportCollectionSchema": {"databases", "read"},
	// UsersService
	"/torchwood.server.v1.UsersService/CreateUser":         {"users", "write"},
	"/torchwood.server.v1.UsersService/ListUsers":          {"users", "read"},
	"/torchwood.server.v1.UsersService/GetUser":            {"users", "read"},
	"/torchwood.server.v1.UsersService/UpdateUser":         {"users", "write"},
	"/torchwood.server.v1.UsersService/UpdateUserPassword": {"users", "write"},
	"/torchwood.server.v1.UsersService/DeleteUser":         {"users", "write"},
	"/torchwood.server.v1.UsersService/ListUserSessions":   {"users", "read"},
	"/torchwood.server.v1.UsersService/DeleteUserSession":  {"users", "write"},
	"/torchwood.server.v1.UsersService/CreateUserToken":    {"users", "write"},
	// GroupsService
	"/torchwood.server.v1.GroupsService/CreateGroup":            {"groups", "write"},
	"/torchwood.server.v1.GroupsService/ListGroups":             {"groups", "read"},
	"/torchwood.server.v1.GroupsService/GetGroup":               {"groups", "read"},
	"/torchwood.server.v1.GroupsService/DeleteGroup":            {"groups", "write"},
	"/torchwood.server.v1.GroupsService/CreateMembership":       {"groups", "write"},
	"/torchwood.server.v1.GroupsService/ListMemberships":        {"groups", "read"},
	"/torchwood.server.v1.GroupsService/GetMembership":          {"groups", "read"},
	"/torchwood.server.v1.GroupsService/UpdateMembership":       {"groups", "write"},
	"/torchwood.server.v1.GroupsService/UpdateMembershipStatus": {"groups", "write"},
	"/torchwood.server.v1.GroupsService/DeleteMembership":       {"groups", "write"},
	"/torchwood.server.v1.GroupsService/GetGroupPrefs":          {"groups", "read"},
	"/torchwood.server.v1.GroupsService/UpdateGroupPrefs":       {"groups", "write"},
	// StorageService
	"/torchwood.server.v1.StorageService/CreateBucket":    {"storage", "write"},
	"/torchwood.server.v1.StorageService/UpdateBucket":    {"storage", "write"},
	"/torchwood.server.v1.StorageService/ListBuckets":     {"storage", "read"},
	"/torchwood.server.v1.StorageService/GetBucket":       {"storage", "read"},
	"/torchwood.server.v1.StorageService/DeleteBucket":    {"storage", "write"},
	"/torchwood.server.v1.StorageService/CreateFile":      {"storage", "write"},
	"/torchwood.server.v1.StorageService/ListFiles":       {"storage", "read"},
	"/torchwood.server.v1.StorageService/GetFile":         {"storage", "read"},
	"/torchwood.server.v1.StorageService/DeleteFile":      {"storage", "write"},
	"/torchwood.server.v1.StorageService/UpdateFile":      {"storage", "write"},
	"/torchwood.server.v1.StorageService/CreateFileToken": {"storage", "write"},
	"/torchwood.server.v1.StorageService/GetStorageUsage": {"storage", "read"},
	// ProjectsService（决策 v8：Create/Delete 为 PERMISSION 平台专属，key 不可调，
	// 不在本表；projects.write 对 key 的实际效力 = 仅 UpdateProject）
	"/torchwood.server.v1.ProjectsService/ListProjects":  {"projects", "read"},
	"/torchwood.server.v1.ProjectsService/GetProject":    {"projects", "read"},
	"/torchwood.server.v1.ProjectsService/UpdateProject": {"projects", "write"},
	// OAuthProvidersService
	"/torchwood.server.v1.OAuthProvidersService/ListOAuthProviders":  {"oauthproviders", "read"},
	"/torchwood.server.v1.OAuthProvidersService/UpsertOAuthProvider": {"oauthproviders", "write"},
	"/torchwood.server.v1.OAuthProvidersService/DeleteOAuthProvider": {"oauthproviders", "write"},
	// APIKeysService 已整体挪 PERMISSION owner/admin（决策 v8）：key 凭证禁入，
	// apikeys scope 资源退役（不再可被授予）。
	// PaymentsService（v3 设计 §6：读 payments.read，写 payments.write）
	"/torchwood.server.v1.PaymentsService/ListOrders":    {"payments", "read"},
	"/torchwood.server.v1.PaymentsService/GetOrder":      {"payments", "read"},
	"/torchwood.server.v1.PaymentsService/Refund":        {"payments", "write"},
	"/torchwood.server.v1.PaymentsService/ManualFulfill": {"payments", "write"},
	// AssetsService（读 assets.read，写 assets.write；economy→assets 决策 v8）
	"/torchwood.server.v1.AssetsService/CreateAssetDef": {"assets", "write"},
	"/torchwood.server.v1.AssetsService/ListAssetDefs":  {"assets", "read"},
	"/torchwood.server.v1.AssetsService/GetAssetDef":    {"assets", "read"},
	"/torchwood.server.v1.AssetsService/UpdateAssetDef": {"assets", "write"},
	"/torchwood.server.v1.AssetsService/DeleteAssetDef": {"assets", "write"},
	"/torchwood.server.v1.AssetsService/Grant":          {"assets", "write"},
	"/torchwood.server.v1.AssetsService/Consume":        {"assets", "write"},
	"/torchwood.server.v1.AssetsService/Transfer":       {"assets", "write"},
	"/torchwood.server.v1.AssetsService/Mutate":         {"assets", "write"},
	"/torchwood.server.v1.AssetsService/Expire":         {"assets", "write"},
	"/torchwood.server.v1.AssetsService/Reconcile":      {"assets", "write"},
	"/torchwood.server.v1.AssetsService/ListUserAssets": {"assets", "read"},
	"/torchwood.server.v1.AssetsService/ListUserLedger": {"assets", "read"},
	// SubscriptionsService（v3 设计 §6：读 subscriptions.read，写 subscriptions.write）
	"/torchwood.server.v1.SubscriptionsService/CreatePlan":         {"subscriptions", "write"},
	"/torchwood.server.v1.SubscriptionsService/ListPlans":          {"subscriptions", "read"},
	"/torchwood.server.v1.SubscriptionsService/GetPlan":            {"subscriptions", "read"},
	"/torchwood.server.v1.SubscriptionsService/UpdatePlan":         {"subscriptions", "write"},
	"/torchwood.server.v1.SubscriptionsService/DeletePlan":         {"subscriptions", "write"},
	"/torchwood.server.v1.SubscriptionsService/ListSubscriptions":  {"subscriptions", "read"},
	"/torchwood.server.v1.SubscriptionsService/GetSubscription":    {"subscriptions", "read"},
	"/torchwood.server.v1.SubscriptionsService/CancelSubscription": {"subscriptions", "write"},
	"/torchwood.server.v1.SubscriptionsService/ExpireSubscription": {"subscriptions", "write"},
	// BillingService（v3 设计 §6：全部只读，scope billing.read / console admin）
	"/torchwood.server.v1.BillingService/GetUsage":       {"billing", "read"},
	"/torchwood.server.v1.BillingService/ListRollups":    {"billing", "read"},
	"/torchwood.server.v1.BillingService/ListStatements": {"billing", "read"},
	// FunctionsService
	"/torchwood.server.v1.FunctionsService/ListRuntimes":       {"functions", "read"},
	"/torchwood.server.v1.FunctionsService/ListSpecifications": {"functions", "read"},
	"/torchwood.server.v1.FunctionsService/CreateFunction":     {"functions", "write"},
	"/torchwood.server.v1.FunctionsService/ListFunctions":      {"functions", "read"},
	"/torchwood.server.v1.FunctionsService/GetFunction":        {"functions", "read"},
	"/torchwood.server.v1.FunctionsService/UpdateFunction":     {"functions", "write"},
	"/torchwood.server.v1.FunctionsService/DeleteFunction":     {"functions", "write"},
	"/torchwood.server.v1.FunctionsService/CreateDeployment":   {"functions", "write"},
	"/torchwood.server.v1.FunctionsService/ListDeployments":    {"functions", "read"},
	"/torchwood.server.v1.FunctionsService/GetDeployment":      {"functions", "read"},
	"/torchwood.server.v1.FunctionsService/DeleteDeployment":   {"functions", "write"},
	"/torchwood.server.v1.FunctionsService/SetVariables":       {"functions", "write"},
	"/torchwood.server.v1.FunctionsService/GetVariables":       {"functions", "read"},
	"/torchwood.server.v1.FunctionsService/CreateExecution":    {"functions", "write"},
	"/torchwood.server.v1.FunctionsService/ListExecutions":     {"functions", "read"},
	"/torchwood.server.v1.FunctionsService/GetExecution":       {"functions", "read"},
	// OutboxService (W-J)
	"/torchwood.server.v1.OutboxService/ListDeadLetters":  {"outbox", "read"},
	"/torchwood.server.v1.OutboxService/ReplayDeadLetter": {"outbox", "write"},
}

// validAPIKeyScopes 是 API key scope 允许的格式全集：{*, all, 裸资源名,
// <resource>.read, <resource>.write}，与 apiKeyScopeRules 单一事实来源一致。
var validAPIKeyScopes = func() map[string]struct{} {
	scopes := map[string]struct{}{"*": {}, "all": {}}
	for _, r := range apiKeyScopeRules {
		scopes[r.resource] = struct{}{}
		scopes[r.resource+".read"] = struct{}{}
		scopes[r.resource+".write"] = struct{}{}
	}
	return scopes
}()

// APIKeyScopeAllowed reports whether scopes grant access to the given gRPC method.
// 匹配规则（B2）：裸资源名全量放行；<resource>.read 仅放行读方法；
// <resource>.write 仅放行写方法；* / all 全量放行。
// 通配符在未映射方法（resource 不存在）之后判断：新增的 ACCESS_API_KEY 服务
// 未登记时即使带 * / all 也拒绝（fail-closed）。
func APIKeyScopeAllowed(fullMethod string, scopes []string) bool {
	rule, ok := apiKeyScopeRules[fullMethod]
	if !ok {
		return false
	}
	for _, s := range scopes {
		if s == "*" || s == "all" {
			return true
		}
		if s == rule.resource {
			return true
		}
		if rule.op == "read" && s == rule.resource+".read" {
			return true
		}
		if rule.op == "write" && s == rule.resource+".write" {
			return true
		}
	}
	return false
}

// ValidAPIKeyScope reports whether a scope string is in the accepted format:
// {*, all, 裸资源名, <resource>.read, <resource>.write}（B2 scope 格式校验）。
func ValidAPIKeyScope(s string) bool {
	_, ok := validAPIKeyScopes[s]
	return ok
}

// IsAPIKeysServiceMethod reports whether fullMethod belongs to the APIKeys service.
// API key 凭证不允许调用这些方法：泄露的 key 若能自铸新 key，等同于永久提权。
// admin console session 不受此限制。
func IsAPIKeysServiceMethod(fullMethod string) bool {
	parts := strings.Split(fullMethod, "/")
	if len(parts) < 2 {
		return false
	}
	seg := parts[len(parts)-2]
	return seg == "APIKeysService" || strings.HasSuffix(seg, ".APIKeysService")
}

// APIKeyScopeRule 是单个 gRPC 方法对应的 scope 资源名与读写方向（B2），
// 导出供启动期一致性断言与工具使用。
type APIKeyScopeRule struct {
	Resource string // scope 资源名（databases/users/groups/storage/projects/oauthproviders/apikeys/functions）
	Op       string // "read" 或 "write"
}

// APIKeyScopeRules 返回 apiKeyScopeRules 的副本（导出访问器）。
func APIKeyScopeRules() map[string]APIKeyScopeRule {
	out := make(map[string]APIKeyScopeRule, len(apiKeyScopeRules))
	for m, r := range apiKeyScopeRules {
		out[m] = APIKeyScopeRule{Resource: r.resource, Op: r.op}
	}
	return out
}

// AssertAPIKeyScopeCoverage 断言 apiKeyScopeRules 覆盖集合与 proto 注解推导出的
// ACCESS_SERVER 方法集合完全一致（R10-P1-5，fail-closed）：
// 不一致直接 panic，并列出缺失（proto 新增方法未登记 scope 规则）与多余
// （规则表残留已删除/改级的方法）。由 server 启动路径调用
// （internal/runtime/grpc.go NewGRPCServer）。
func AssertAPIKeyScopeCoverage(apiKeyMethods []string) {
	expected := make(map[string]struct{}, len(apiKeyMethods))
	for _, m := range apiKeyMethods {
		expected[m] = struct{}{}
	}
	actual := apiKeyScopeRules

	var missing, extra []string
	for m := range expected {
		if _, ok := actual[m]; !ok {
			missing = append(missing, m)
		}
	}
	for m := range actual {
		if _, ok := expected[m]; !ok {
			extra = append(extra, m)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	sort.Strings(missing)
	sort.Strings(extra)
	panic(fmt.Sprintf("apiKeyScopeRules 与 ACCESS_SERVER 方法集合不一致 (fail-closed): "+
		"proto 声明但规则表缺失=%v; 规则表多余=%v", missing, extra))
}
