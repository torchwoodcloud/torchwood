package auth

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lynx-go/grpcapi/authz"
)

// 本文件是授权策略注册表的 domain 层（阶段 1 起类型本体移驻
// github.com/lynx-go/grpcapi/authz，此处保留 torchwood 词表、档位分类与
// 语义断言——值域因项目而异的部分按设计留在项目侧）：
// 策略唯一声明在 proto（grpcapi.v1 method_auth/service_auth），由
// cmd/server/internal/runtime 的收集入口（buildMethodPolicies）从 descriptor
// 构造 authz.PolicySet 注入各执行点。domain 不依赖 genproto（AGENTS.md
// 分层约定）；authz 包零 grpc/lynx 依赖，可安全被 domain 引用。

// —— 类型别名（grpcapi.authz 本体）——

type (
	// PolicySet 是全量方法策略注册表（由 runtime 收集器构造，注入执行点）。
	PolicySet = authz.PolicySet
	// MethodPolicy 是单个方法的完整授权策略（proto 声明的运行时投影）。
	// AdminRoles 为 []string（词表主权在项目，见下方 AdminRole 词表）；
	// RequestFields 为输入消息排序全字段投影（project_id 寻址不变量消费）。
	MethodPolicy = authz.MethodPolicy
	// AccessLevel 是方法的凭证族门禁（与 proto grpcapi.v1.AccessLevel 一一对应）。
	AccessLevel = authz.AccessLevel
	// ScopeRule 是 SERVER 面方法对 API key 凭证开放的 scope 门。
	ScopeRule = authz.ScopeRule
	// ScopeOp 是 scope 的权限方向。
	ScopeOp = authz.ScopeOp
)

const (
	AccessLevelUnspecified AccessLevel = authz.AccessUnspecified
	// AccessPublic 匿名可调（自证凭证型）。
	AccessPublic AccessLevel = authz.AccessPublic
	// AccessEndUser 端用户会话/JWT 专属（Client 面）。
	AccessEndUser AccessLevel = authz.AccessEndUser
	// AccessServer admin 会话（admin_roles）或 API key（scope）。
	AccessServer AccessLevel = authz.AccessServer
	// AccessPermission admin 会话专属（permissions），key 一律拒绝。
	AccessPermission AccessLevel = authz.AccessPermission
	// AccessSystem 内部系统调用（预留）。
	AccessSystem AccessLevel = authz.AccessSystem
)

const (
	ScopeRead  ScopeOp = authz.ScopeRead
	ScopeWrite ScopeOp = authz.ScopeWrite
	ScopeAdmin ScopeOp = authz.ScopeAdmin
)

// AdminRole 是 console admin 的 RBAC 角色（平台级角色体系；角色串即
// principal.Roles 中的形态——proto 注解侧为 string，值域由本词表锁定）。
type AdminRole string

const (
	AdminRoleViewer AdminRole = "viewer"
	AdminRoleMember AdminRole = "member"
	AdminRoleAdmin  AdminRole = "admin"
	AdminRoleOwner  AdminRole = "owner"
)

// RoleConsoleTag 是 admin 会话标签（非角色）：所有 admin 会话主体恒持有，
// 供 console 面 me 型方法（GetCurrentAdmin）放行。不进 AdminRole 词表。
const RoleConsoleTag = "console"

// RoleEndUserTag 是端用户基础角色：Client 面方法的 permissions 恒为
// ["users"]（值域断言锁定），端用户角色解析恒含该角色。
const RoleEndUserTag = "users"

// AllAdminRoles 是 AdminRole 全集（词表主权：档位断言与测试引用）。
var AllAdminRoles = []AdminRole{AdminRoleViewer, AdminRoleMember, AdminRoleAdmin, AdminRoleOwner}

// ScopeResource 是 API key scope 的资源词表（收集期 Vocabulary 注册与
// 死 scope 断言的对照面；proto 注解侧为 string，值域由本词表锁定）。
type ScopeResource string

const (
	ScopeDatabases      ScopeResource = "databases"
	ScopeUsers          ScopeResource = "users"
	ScopeGroups         ScopeResource = "groups"
	ScopeStorage        ScopeResource = "storage"
	ScopeProjects       ScopeResource = "projects"
	ScopeOAuthProviders ScopeResource = "oauthproviders"
	ScopeFunctions      ScopeResource = "functions"
	ScopePayments       ScopeResource = "payments"
	ScopeAssets         ScopeResource = "assets"
	ScopeSubscriptions  ScopeResource = "subscriptions"
	ScopeBilling        ScopeResource = "billing"
	ScopeOutbox         ScopeResource = "outbox"
	ScopeAuditLogs      ScopeResource = "audit_logs"
	ScopeLeaderboards   ScopeResource = "leaderboards"
	ScopeAnalytics      ScopeResource = "analytics"
	ScopeRunbooks       ScopeResource = "runbooks"
	ScopeRuntimeVars    ScopeResource = "runtime_vars"
)

// AllScopeResources 是资源词表全集（收集期 Vocabulary 与死 scope 断言的对照面）。
var AllScopeResources = []ScopeResource{
	ScopeDatabases, ScopeUsers, ScopeGroups, ScopeStorage, ScopeProjects,
	ScopeOAuthProviders, ScopeFunctions, ScopePayments, ScopeAssets,
	ScopeSubscriptions, ScopeBilling, ScopeOutbox, ScopeAuditLogs,
	ScopeLeaderboards, ScopeAnalytics, ScopeRunbooks, ScopeRuntimeVars,
}

// ScopeVocabularyResources 是收集期注册给 grpcapi.authz.Build 的词表
// （string 形态；资源在词表内为不可关闭的内置断言）。
func ScopeVocabularyResources() []string {
	out := make([]string, 0, len(AllScopeResources))
	for _, r := range AllScopeResources {
		out = append(out, string(r))
	}
	return out
}

// AllowedAdminRoles 返回 SERVER 面方法允许的 admin 角色集（非 SERVER 面
// 或未声明返回 nil；nil 语义 = 不限角色，viewer 可调）。供 serverhttp/
// realtime 等镜像消费点派生，禁止手写角色集。
func AllowedAdminRoles(s *PolicySet, fullMethod string) []string {
	return s.AllowedAdminRoles(fullMethod)
}

// ScopeVocabulary 是从 PolicySet 派生的合法 scope 词表（创建校验与
// well-known/SDK/console 下发的单一来源）。词表 = 全部被引用资源的
// {资源名, 资源名.read, 资源名.write} ∪ {*, all}。
type ScopeVocabulary struct {
	valid     map[string]struct{}
	resources []ScopeResource
	ops       map[ScopeResource]map[ScopeOp]struct{}
}

// VocabularyFromPolicies 从策略注册表派生词表（死 scope 断言保证每个资源
// 至少被引用一次，因此 resources 恒非空）。
func VocabularyFromPolicies(set *PolicySet) *ScopeVocabulary {
	v := &ScopeVocabulary{valid: map[string]struct{}{"*": {}, "all": {}}, ops: map[ScopeResource]map[ScopeOp]struct{}{}}
	seen := map[ScopeResource]struct{}{}
	for _, p := range set.Methods() {
		if p.Scope == nil {
			continue
		}
		res := ScopeResource(p.Scope.Resource)
		if _, ok := seen[res]; !ok {
			seen[res] = struct{}{}
			v.resources = append(v.resources, res)
		}
		if v.ops[res] == nil {
			v.ops[res] = map[ScopeOp]struct{}{}
		}
		v.ops[res][p.Scope.Op] = struct{}{}
		v.valid[p.Scope.Resource] = struct{}{}
		v.valid[p.Scope.Resource+"."+string(p.Scope.Op)] = struct{}{}
	}
	sort.Slice(v.resources, func(i, j int) bool { return v.resources[i] < v.resources[j] })
	return v
}

// Valid 报告 scope 字符串是否在词表内（key 创建校验用）。合法形态三类：
//   - 内建精确形态：{*, all} ∪ {资源, 资源.op}；
//   - 内建实例限定形态：可寻址资源（databases/storage）的
//     <res>:<id>[.op]——资源需在词表、方向需被声明、目标 ID 格式合法（T-02）；
//   - 自定义服务形态：<service>.<name>（T2，纯语法门 + 内建保护；TW 不解释
//     其语义，执行期对 TW 方法永不匹配）。
func (v *ScopeVocabulary) Valid(s string) bool {
	if v == nil {
		return false
	}
	if _, ok := v.valid[s]; ok {
		return true
	}
	if tok, ok := ParseScopeToken(s); ok && tok.TargetID != "" {
		if !ScopeAddressable(tok.Resource) {
			return false
		}
		if _, ok := v.valid[string(tok.Resource)]; !ok {
			return false
		}
		if tok.Op != "" && !v.HasOp(tok.Resource, tok.Op) {
			return false
		}
		return ValidateScopeTargetID(tok.Resource, tok.TargetID) == nil
	}
	_, _, ok := ParseCustomScope(s)
	return ok
}

// Resources 返回被引用的资源清单（排序稳定，供下发与生成）。
func (v *ScopeVocabulary) Resources() []ScopeResource {
	if v == nil {
		return nil
	}
	return append([]ScopeResource{}, v.resources...)
}

// HasOp 报告资源是否声明了该方向（供下发面描述资源能力）。
func (v *ScopeVocabulary) HasOp(r ScopeResource, op ScopeOp) bool {
	if v == nil {
		return false
	}
	ops, ok := v.ops[r]
	if !ok {
		return false
	}
	_, has := ops[op]
	return has
}

// Tier 是 SERVER/PERMISSION 面方法的档位（从声明派生，非独立声明维度——
// 机制裁决：档位是 Classify 纯函数的输出，不进 proto，避免第二策略源）。
type Tier string

const (
	// TierReadOnly 读面：scope=read，不限 admin 角色（viewer 可调）。
	TierReadOnly Tier = "read_only"
	// TierBusinessWrite 业务写：scope=write 且 member 可调（member/admin/owner）。
	TierBusinessWrite Tier = "business_write"
	// TierDelegatedPlatform 委托平台档：仅 admin/owner 角色 + 对应 scope 的
	// key 通道（DDL/Functions/Assets 动词/退款/死信等"委托自动化"面）。
	TierDelegatedPlatform Tier = "delegated_platform"
	// TierPlatformOnly 平台专属：PERMISSION 面，perms ⊆ {admin,owner}，
	// 无 key 通道（项目建删、API key 管理）。
	TierPlatformOnly Tier = "platform_only"
)

// roleSet 规范化角色集合比较（顺序无关）。
func roleSet(roles []string) map[string]struct{} {
	m := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		m[r] = struct{}{}
	}
	return m
}

// ClassifyTier 从策略声明派生档位。返回错误 = 声明组合不属于任何已声明
// 档位（如 write+空角色、roles 含 viewer、PERMISSION+member 等）——新档位
// 必须显式扩展本函数（classify 是档位的唯一机械定义）。
func ClassifyTier(p MethodPolicy) (Tier, error) {
	switch p.Access {
	case AccessServer:
		if p.Scope == nil {
			return "", fmt.Errorf("method %s: SERVER access requires api_key_scope", p.Method)
		}
		roles := roleSet(p.AdminRoles)
		platformOnly := len(roles) == 2 && hasRole(roles, string(AdminRoleAdmin)) && hasRole(roles, string(AdminRoleOwner))
		business := len(roles) == 3 && hasRole(roles, string(AdminRoleMember)) && hasRole(roles, string(AdminRoleAdmin)) && hasRole(roles, string(AdminRoleOwner))
		unrestricted := len(roles) == 0
		switch {
		case platformOnly:
			return TierDelegatedPlatform, nil
		case unrestricted && p.Scope.Op == ScopeRead:
			return TierReadOnly, nil
		case business && p.Scope.Op == ScopeWrite:
			return TierBusinessWrite, nil
		default:
			return "", fmt.Errorf("method %s: (admin_roles=%v, scope=%s.%s) matches no declared tier (read+any roles / write+{member,admin,owner} / {admin,owner}+read-write)",
				p.Method, p.AdminRoles, p.Scope.Resource, p.Scope.Op)
		}
	case AccessPermission:
		for _, perm := range p.Permissions {
			switch AdminRole(perm) {
			case AdminRoleAdmin, AdminRoleOwner:
			default:
				return "", fmt.Errorf("method %s: PERMISSION access allows only owner/admin (got %q)", p.Method, perm)
			}
		}
		return TierPlatformOnly, nil
	default:
		return "", fmt.Errorf("method %s: tiers are defined only for SERVER/PERMISSION access", p.Method)
	}
}

func hasRole(set map[string]struct{}, role string) bool {
	_, ok := set[role]
	return ok
}

// ProjectIDAllowlist 是 server 面请求体携带 project_id 字段的存量迁移
// 白名单（B2 评审裁决：断言 + 白名单渐进清空，新增即启动失败）。
// ProjectsService 自身豁免（项目是资源本体，非寻址参数）。
var ProjectIDAllowlist = map[string]struct{}{
	"/torchwood.server.v1.BillingService/GetUsage":                 {},
	"/torchwood.server.v1.BillingService/ListRollups":              {},
	"/torchwood.server.v1.BillingService/ListStatements":           {},
	"/torchwood.server.v1.PaymentsService/GetOrder":                {},
	"/torchwood.server.v1.FunctionsService/CreateExecution":        {},
	"/torchwood.server.v1.SubscriptionsService/CancelSubscription": {},
	"/torchwood.server.v1.SubscriptionsService/ExpireSubscription": {},
	"/torchwood.server.v1.AssetsService/ListUserAssets":            {},
	"/torchwood.server.v1.AssetsService/ListUserLedger":            {},
}

// requestHasProjectID 从排序字段投影判断 project_id 寻址（grpcapi
// MethodPolicy.RequestFields 的消费面）。
func requestHasProjectID(p MethodPolicy) bool {
	for _, f := range p.RequestFields {
		if f == "project_id" {
			return true
		}
	}
	return false
}

// AssertPolicy 是单方法语义断言（完备性/档位/值域/项目寻址；不含死 scope
// 与 streaming 全局项）——供逐方法测试与矩阵测试复用。
func AssertPolicy(p MethodPolicy) error {
	var errs []string
	switch p.Access {
	case AccessPublic:
	case AccessEndUser:
		if !isClientFace(p.Service) {
			errs = append(errs, fmt.Sprintf("%s: END_USER access is allowed only on the client face", p.Method))
		}
	case AccessServer:
		if p.Scope == nil {
			errs = append(errs, fmt.Sprintf("%s: SERVER access must declare api_key_scope (use PERMISSION if not exposed to API keys)", p.Method))
			break
		}
		if _, badScope := validScope(*p.Scope); badScope != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.Method, badScope))
		}
		if _, err := ClassifyTier(p); err != nil {
			errs = append(errs, err.Error())
		}
	case AccessPermission:
		if len(p.Permissions) == 0 {
			errs = append(errs, fmt.Sprintf("%s: PERMISSION access must declare explicit permissions", p.Method))
			break
		}
		if isServerFace(p.Service) {
			if _, err := ClassifyTier(p); err != nil {
				errs = append(errs, err.Error())
			}
		}
	case AccessSystem:
		errs = append(errs, fmt.Sprintf("%s: SYSTEM access has no external contract yet and is forbidden", p.Method))
	default:
		errs = append(errs, fmt.Sprintf("%s: access is not declared", p.Method))
	}
	if requestHasProjectID(p) && isServerFace(p.Service) && !isProjectsService(p.Service) {
		if _, ok := ProjectIDAllowlist[p.Method]; !ok {
			errs = append(errs, fmt.Sprintf("%s: server-face request body must not carry project_id (project context comes from credentials; for legacy migration register in ProjectIDAllowlist)", p.Method))
		}
	}
	if isConsoleFace(p.Service) {
		if err := assertConsoleValueDomain(p); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if isClientFace(p.Service) {
		if err := assertClientValueDomain(p); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("policy semantic assertion failed (fail-closed):\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// AssertSemantic 是 PolicySet 的全量语义断言（启动期 fail-closed）：
// 完备性、死 scope、档位合法性、client/console 值域、项目寻址不变量、
// streaming 禁用。返回的 error 聚合全部违例。
func AssertSemantic(set *PolicySet) error {
	var errs []string
	referenced := map[string]struct{}{}

	for _, p := range set.Methods() {
		if err := AssertPolicy(p); err != nil {
			errs = append(errs, err.Error())
		}
		if p.Scope != nil {
			referenced[p.Scope.Resource] = struct{}{}
		}
		if p.IsStreaming {
			errs = append(errs, fmt.Sprintf("%s: streaming RPC is not wired into the auth interceptor (fail-closed)", p.Method))
		}
	}

	// 死 scope 检测：词表内每个资源必须被至少一个方法引用。
	for _, r := range AllScopeResources {
		if _, ok := referenced[string(r)]; !ok {
			errs = append(errs, fmt.Sprintf("dead scope: resource %q is not referenced by any method (leftover from vocabulary evolution)", r))
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("policy semantic assertion failed (fail-closed):\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// clientPublicMethodWhitelist 是 client 面 PUBLIC 方法的显式白名单
// （自证凭证型/匿名读语义）。新增 PUBLIC 方法必须显式登记（防误标）。
var clientPublicMethodWhitelist = map[string]struct{}{
	"/torchwood.client.v1.AccountService/SignUp":                         {},
	"/torchwood.client.v1.AccountService/SignIn":                         {},
	"/torchwood.client.v1.AccountService/RefreshToken":                   {},
	"/torchwood.client.v1.AccountService/ConfirmEmailChange":             {},
	"/torchwood.client.v1.AccountService/CreateEmailOTP":                 {},
	"/torchwood.client.v1.AccountService/CreateEmailOTPSession":          {},
	"/torchwood.client.v1.AccountService/CreateOAuth2Session":            {},
	"/torchwood.client.v1.AccountService/CreateOAuth2TokenSession":       {},
	"/torchwood.client.v1.AccountService/CreatePhoneOTP":                 {},
	"/torchwood.client.v1.AccountService/CreatePhoneOTPSession":          {},
	"/torchwood.client.v1.AccountService/CreateWeChatMiniProgramSession": {},
	"/torchwood.client.v1.AccountService/CreateAnonymousSession":         {},
	"/torchwood.client.v1.AccountService/UpdateVerification":             {},
	"/torchwood.client.v1.AccountService/CreateRecovery":                 {},
	"/torchwood.client.v1.AccountService/UpdateRecovery":                 {},
	"/torchwood.client.v1.AccountService/CreateMFASession":               {},
	"/torchwood.client.v1.AccountService/CreateMagicURLSession":          {},
	"/torchwood.client.v1.AccountService/UpdateMagicURLSession":          {},
	"/torchwood.client.v1.DatabasesService/ListDocuments":                {},
	"/torchwood.client.v1.DatabasesService/GetDocument":                  {},
	"/torchwood.client.v1.DatabasesService/CountDocuments":               {},
	// RuntimeVars client 面（docs/design/runtime-vars.md §2.3）：public 集合
	// 匿名可读；private 集合的可见性过滤在数据访问层（RuntimeVarPublicRead）。
	"/torchwood.client.v1.RuntimeVarsService/GetRuntimeVars": {},
}

func assertClientValueDomain(p MethodPolicy) error {
	if p.Access == AccessPublic {
		if _, ok := clientPublicMethodWhitelist[p.Method]; !ok {
			return fmt.Errorf("%s: client-face PUBLIC method must be explicitly allowlisted (guards against accidental public exposure)", p.Method)
		}
		return nil
	}
	if p.Access != AccessEndUser {
		return fmt.Errorf("%s: client face allows only PUBLIC/END_USER access", p.Method)
	}
	if len(p.Permissions) != 1 || p.Permissions[0] != RoleEndUserTag {
		return fmt.Errorf("%s: client-face END_USER method permissions must be exactly [%q]", p.Method, RoleEndUserTag)
	}
	return nil
}

// consoleSelfServiceWriteWhitelist 是 console 面"写动词命名但仅作用于调用者
// 自身记录"的自助方法显式登记（写动词必须 [owner] 规则的豁免口，与
// clientPublicMethodWhitelist 同模式防误标扩散）。登记前提：方法 self-scoped
// ——目标 id 只能取自 principal（callerID），不接受路径/请求体里的第三方 id。
var consoleSelfServiceWriteWhitelist = map[string]struct{}{
	"/torchwood.console.v1.AdminsService/UpdateCurrentAdmin": {},
}

func assertConsoleValueDomain(p MethodPolicy) error {
	if p.Access == AccessPublic {
		return nil // ConsoleAuthService 的自证凭证型公开方法
	}
	if p.Access != AccessPermission {
		return fmt.Errorf("%s: console face allows only PUBLIC/PERMISSION access", p.Method)
	}
	perms := strings.Join(p.Permissions, ",")
	switch perms {
	case RoleConsoleTag, "owner", "owner,admin", "admin,owner":
	default:
		return fmt.Errorf("%s: console-face permissions must be one of [console]/[owner]/[owner,admin] (got %q)", p.Method, perms)
	}
	// 写动词方法最严：仅 owner（自助偏好类方法经白名单显式豁免）。
	name := p.Method[strings.LastIndex(p.Method, "/")+1:]
	if strings.HasPrefix(name, "Create") || strings.HasPrefix(name, "Update") || strings.HasPrefix(name, "Delete") {
		if _, selfService := consoleSelfServiceWriteWhitelist[p.Method]; !selfService && perms != "owner" {
			return fmt.Errorf("%s: console-face write method permissions must be [owner] (got %q)", p.Method, perms)
		}
	}
	return nil
}

func validScope(rule ScopeRule) (ScopeRule, error) {
	validRes := false
	for _, r := range AllScopeResources {
		if rule.Resource == string(r) {
			validRes = true
			break
		}
	}
	if !validRes {
		return rule, fmt.Errorf("scope resource %q is not in the vocabulary", rule.Resource)
	}
	if rule.Op != ScopeRead && rule.Op != ScopeWrite && rule.Op != ScopeAdmin {
		return rule, fmt.Errorf("invalid scope op %q", rule.Op)
	}
	return rule, nil
}

func isClientFace(service string) bool  { return strings.HasPrefix(service, "/torchwood.client.v1.") }
func isConsoleFace(service string) bool { return strings.HasPrefix(service, "/torchwood.console.v1.") }
func isServerFace(service string) bool  { return strings.HasPrefix(service, "/torchwood.server.v1.") }
func isProjectsService(service string) bool {
	return strings.HasPrefix(service, "/torchwood.server.v1.ProjectsService")
}
