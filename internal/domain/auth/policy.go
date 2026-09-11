package auth

import (
	"fmt"
	"sort"
	"strings"
)

// 本文件是授权策略注册表的 domain 纯类型层（机制重设计 M2）：
// 策略唯一声明在 proto（authz.proto method_auth/service_auth），由
// cmd/server/internal/runtime 的收集器（BuildMethodPolicies）从 descriptor 构造
// PolicySet 注入各执行点。domain 不依赖 genproto（AGENTS.md 分层约定），
// 因此这里只定义类型、档位分类与语义断言——全部是输入 PolicySet 的纯函数。

// AccessLevel 是方法的凭证族门禁（与 proto shared.v1.AccessLevel 一一对应）。
type AccessLevel int

const (
	AccessLevelUnspecified AccessLevel = 0
	AccessPublic           AccessLevel = 1 // 匿名可调（自证凭证型）
	AccessEndUser          AccessLevel = 2 // 端用户会话/JWT 专属（Client 面）
	AccessServer           AccessLevel = 3 // admin 会话（admin_roles）或 API key（scope）
	AccessPermission       AccessLevel = 4 // admin 会话专属（permissions），key 一律拒绝
	AccessSystem           AccessLevel = 5 // 内部系统调用（预留）
)

// AdminRole 是 console admin 的 RBAC 角色（平台级角色体系，与 proto
// shared.v1.AdminRole enum 一一对应；角色串即 principal.Roles 中的形态）。
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

// ScopeResource 是 API key scope 的资源词表（与 proto shared.v1.ScopeResource
// 一一对应；economy 已更名 assets，apikeys 资源已退役）。
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
)

// AllScopeResources 是资源词表全集（死 scope 断言的对照面）。
var AllScopeResources = []ScopeResource{
	ScopeDatabases, ScopeUsers, ScopeGroups, ScopeStorage, ScopeProjects,
	ScopeOAuthProviders, ScopeFunctions, ScopePayments, ScopeAssets,
	ScopeSubscriptions, ScopeBilling, ScopeOutbox, ScopeAuditLogs,
	ScopeLeaderboards,
	ScopeSubscriptions, ScopeBilling, ScopeOutbox, ScopeAuditLogs, ScopeAnalytics,
}

// ScopeOp 是 scope 的读写方向。
type ScopeOp string

const (
	ScopeRead  ScopeOp = "read"
	ScopeWrite ScopeOp = "write"
)

// ScopeRule 是 SERVER 面方法对 API key 凭证开放的 scope 门。
type ScopeRule struct {
	Resource ScopeResource
	Op       ScopeOp
}

// MethodPolicy 是单个方法的完整授权策略（proto 声明的运行时投影）。
type MethodPolicy struct {
	Method      string      // full method，如 "/torchwood.server.v1.UsersService/CreateUser"
	Service     string      // full service，如 "/torchwood.server.v1.UsersService"
	Access      AccessLevel // 凭证族
	Permissions []string    // PERMISSION 面角色门（小写角色名 / console 标签 / users）
	AdminRoles  []AdminRole // SERVER 面 admin 会话角色门；空 = 不限角色（viewer 可调，仅读方法）
	Scope       *ScopeRule  // SERVER 面 API key 门；nil = 不对 key 开放
	// RequestHasProjectID 为 true 表示请求消息含 project_id 字段。项目寻址
	// 不变量：server 面请求体不得携带项目寻址（项目上下文一律来自凭证），
	// 存量违例必须登记在 ProjectIDAllowlist（迁移白名单，按序清空）。
	RequestHasProjectID bool
	IsStreaming         bool // fail-closed：当前无 stream RPC，出现即断言失败
}

// PolicySet 是全量方法策略注册表（由 runtime 收集器构造，注入执行点）。
type PolicySet struct {
	methods map[string]MethodPolicy
}

// NewPolicySet 构造注册表（重复方法名返回错误）。
func NewPolicySet(policies []MethodPolicy) (*PolicySet, error) {
	m := make(map[string]MethodPolicy, len(policies))
	for _, p := range policies {
		if _, dup := m[p.Method]; dup {
			return nil, fmt.Errorf("duplicate method policy %s", p.Method)
		}
		m[p.Method] = p
	}
	return &PolicySet{methods: m}, nil
}

// Get 返回单方法策略。
func (s *PolicySet) Get(fullMethod string) (MethodPolicy, bool) {
	if s == nil {
		return MethodPolicy{}, false
	}
	p, ok := s.methods[fullMethod]
	return p, ok
}

// Methods 返回全部策略（按方法名排序，供矩阵测试与文档生成遍历）。
func (s *PolicySet) Methods() []MethodPolicy {
	out := make([]MethodPolicy, 0, len(s.methods))
	for _, p := range s.methods {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method < out[j].Method })
	return out
}

// AllowedAdminRoles 返回 SERVER 面方法允许的 admin 角色集（非 SERVER 面
// 或未声明返回 nil；nil 语义 = 不限角色，viewer 可调）。供 serverhttp/
// realtime 等镜像消费点派生，禁止手写角色集。
func (s *PolicySet) AllowedAdminRoles(fullMethod string) []AdminRole {
	p, ok := s.Get(fullMethod)
	if !ok || p.Access != AccessServer {
		return nil
	}
	return p.AdminRoles
}

// HasAPIKeyScope 返回方法对 API key 开放的 scope 规则（未开放返回 nil）。
func (s *PolicySet) HasAPIKeyScope(fullMethod string) *ScopeRule {
	p, ok := s.Get(fullMethod)
	if !ok || p.Access != AccessServer {
		return nil
	}
	return p.Scope
}

// AllowsAPIKey 判定给定 scope 集合是否放行该方法（B2 匹配语义；T-02 起为
// AllowsAPIKeyTargets 零目标的退化形态——无实例寻址时资源限定 scope 恒不
// 匹配，裸资源/.op/通配符行为不变）。未声明 scope 的方法（非 SERVER 面或
// 平台专属）一律拒绝——fail-closed，与通配符无关。
func (s *PolicySet) AllowsAPIKey(fullMethod string, scopes []string) bool {
	return s.AllowsAPIKeyTargets(fullMethod, scopes, ScopeTargets{})
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
		if _, ok := seen[p.Scope.Resource]; !ok {
			seen[p.Scope.Resource] = struct{}{}
			v.resources = append(v.resources, p.Scope.Resource)
		}
		if v.ops[p.Scope.Resource] == nil {
			v.ops[p.Scope.Resource] = map[ScopeOp]struct{}{}
		}
		v.ops[p.Scope.Resource][p.Scope.Op] = struct{}{}
		v.valid[string(p.Scope.Resource)] = struct{}{}
		v.valid[string(p.Scope.Resource)+"."+string(p.Scope.Op)] = struct{}{}
	}
	sort.Slice(v.resources, func(i, j int) bool { return v.resources[i] < v.resources[j] })
	return v
}

// Valid 报告 scope 字符串是否在词表内（key 创建校验用）。除既有精确形态
// （{*, all} ∪ {资源, 资源.op}）外，接受可寻址资源（databases/storage）的
// 实例限定形态 <res>:<id>[.op]——资源需在词表、方向需被声明、目标 ID 格式
// 合法（T-02）。
func (v *ScopeVocabulary) Valid(s string) bool {
	if v == nil {
		return false
	}
	if _, ok := v.valid[s]; ok {
		return true
	}
	tok, ok := ParseScopeToken(s)
	if !ok || tok.TargetID == "" {
		return false
	}
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
// 机制裁决：档位是 classify 纯函数的输出，不进 proto，避免第二策略源）。
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

// RoleStrings 将 AdminRole 集合转为主体角色串集合（AdminRole 的字符串
// 形态即 principal.Roles 中的角色串，两者由本包词表锁定一致）。供
// HasAnyRole 消费点把策略角色门转为主体角色匹配。
func RoleStrings(roles []AdminRole) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return out
}

// roleSet 规范化角色集合比较（顺序无关）。
func roleSet(roles []AdminRole) map[AdminRole]struct{} {
	m := make(map[AdminRole]struct{}, len(roles))
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
			return "", fmt.Errorf("method %s: SERVER 面缺 api_key_scope", p.Method)
		}
		roles := roleSet(p.AdminRoles)
		platformOnly := len(roles) == 2 && hasRole(roles, AdminRoleAdmin) && hasRole(roles, AdminRoleOwner)
		business := len(roles) == 3 && hasRole(roles, AdminRoleMember) && hasRole(roles, AdminRoleAdmin) && hasRole(roles, AdminRoleOwner)
		unrestricted := len(roles) == 0
		switch {
		case platformOnly:
			return TierDelegatedPlatform, nil
		case unrestricted && p.Scope.Op == ScopeRead:
			return TierReadOnly, nil
		case business && p.Scope.Op == ScopeWrite:
			return TierBusinessWrite, nil
		default:
			return "", fmt.Errorf("method %s: (admin_roles=%v, scope=%s.%s) 不属于任何已声明档位（read+不限角色 / write+{member,admin,owner} / {admin,owner}+读写）",
				p.Method, p.AdminRoles, p.Scope.Resource, p.Scope.Op)
		}
	case AccessPermission:
		for _, perm := range p.Permissions {
			switch AdminRole(perm) {
			case AdminRoleAdmin, AdminRoleOwner:
			default:
				return "", fmt.Errorf("method %s: PERMISSION 面只允许 owner/admin（得到 %q）", p.Method, perm)
			}
		}
		return TierPlatformOnly, nil
	default:
		return "", fmt.Errorf("method %s: 档位仅定义在 SERVER/PERMISSION 面", p.Method)
	}
}

func hasRole(set map[AdminRole]struct{}, role AdminRole) bool {
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

// AssertPolicy 是单方法语义断言（完备性/档位/值域/项目寻址；不含死 scope
// 与 streaming 全局项）——供逐方法测试与矩阵测试复用。
func AssertPolicy(p MethodPolicy) error {
	var errs []string
	switch p.Access {
	case AccessPublic:
	case AccessEndUser:
		if !isClientFace(p.Service) {
			errs = append(errs, fmt.Sprintf("%s: END_USER 级仅允许 client 面", p.Method))
		}
	case AccessServer:
		if p.Scope == nil {
			errs = append(errs, fmt.Sprintf("%s: SERVER 面必须声明 api_key_scope（不对 key 开放请改 PERMISSION）", p.Method))
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
			errs = append(errs, fmt.Sprintf("%s: PERMISSION 面必须显式 permissions", p.Method))
			break
		}
		if isServerFace(p.Service) {
			if _, err := ClassifyTier(p); err != nil {
				errs = append(errs, err.Error())
			}
		}
	case AccessSystem:
		errs = append(errs, fmt.Sprintf("%s: SYSTEM 级当前无对外契约，禁止使用", p.Method))
	default:
		errs = append(errs, fmt.Sprintf("%s: access 未声明", p.Method))
	}
	if p.RequestHasProjectID && isServerFace(p.Service) && !isProjectsService(p.Service) {
		if _, ok := ProjectIDAllowlist[p.Method]; !ok {
			errs = append(errs, fmt.Sprintf("%s: server 面请求体不得携带 project_id（项目上下文来自凭证；存量迁移请登记 ProjectIDAllowlist）", p.Method))
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
		return fmt.Errorf("策略语义断言失败 (fail-closed):\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// AssertSemantic 是 PolicySet 的全量语义断言（启动期 fail-closed）：
// 完备性、死 scope、档位合法性、client/console 值域、项目寻址不变量、
// streaming 禁用。返回的 error 聚合全部违例。
func AssertSemantic(set *PolicySet) error {
	var errs []string
	referenced := map[ScopeResource]struct{}{}

	for _, p := range set.Methods() {
		if err := AssertPolicy(p); err != nil {
			errs = append(errs, err.Error())
		}
		if p.Scope != nil {
			referenced[p.Scope.Resource] = struct{}{}
		}
		if p.IsStreaming {
			errs = append(errs, fmt.Sprintf("%s: streaming RPC 未接入认证拦截器（fail-closed）", p.Method))
		}
	}

	// 死 scope 检测：词表内每个资源必须被至少一个方法引用。
	for _, r := range AllScopeResources {
		if _, ok := referenced[r]; !ok {
			errs = append(errs, fmt.Sprintf("死 scope：资源 %q 未被任何方法引用（词表演进后残留）", r))
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("策略语义断言失败 (fail-closed):\n  - %s", strings.Join(errs, "\n  - "))
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
}

func assertClientValueDomain(p MethodPolicy) error {
	if p.Access == AccessPublic {
		if _, ok := clientPublicMethodWhitelist[p.Method]; !ok {
			return fmt.Errorf("%s: client 面 PUBLIC 方法必须显式登记白名单（防误标公开）", p.Method)
		}
		return nil
	}
	if p.Access != AccessEndUser {
		return fmt.Errorf("%s: client 面只允许 PUBLIC/END_USER 级", p.Method)
	}
	if len(p.Permissions) != 1 || p.Permissions[0] != RoleEndUserTag {
		return fmt.Errorf("%s: client 面 END_USER 方法 permissions 必须恰为 [%q]", p.Method, RoleEndUserTag)
	}
	return nil
}

func assertConsoleValueDomain(p MethodPolicy) error {
	if p.Access == AccessPublic {
		return nil // ConsoleAuthService 的自证凭证型公开方法
	}
	if p.Access != AccessPermission {
		return fmt.Errorf("%s: console 面只允许 PUBLIC/PERMISSION 级", p.Method)
	}
	perms := strings.Join(p.Permissions, ",")
	switch perms {
	case RoleConsoleTag, "owner", "owner,admin", "admin,owner":
	default:
		return fmt.Errorf("%s: console 面 permissions 值域为 [console]/[owner]/[owner,admin]（得到 %q）", p.Method, perms)
	}
	// 写动词方法最严：仅 owner。
	name := p.Method[strings.LastIndex(p.Method, "/")+1:]
	if strings.HasPrefix(name, "Create") || strings.HasPrefix(name, "Update") || strings.HasPrefix(name, "Delete") {
		if perms != "owner" {
			return fmt.Errorf("%s: console 面写方法 permissions 必须为 [owner]（得到 %q）", p.Method, perms)
		}
	}
	return nil
}

func validScope(rule ScopeRule) (ScopeRule, error) {
	validRes := false
	for _, r := range AllScopeResources {
		if rule.Resource == r {
			validRes = true
			break
		}
	}
	if !validRes {
		return rule, fmt.Errorf("scope 资源 %q 不在词表", rule.Resource)
	}
	if rule.Op != ScopeRead && rule.Op != ScopeWrite {
		return rule, fmt.Errorf("scope 方向 %q 非法", rule.Op)
	}
	return rule, nil
}

func isClientFace(service string) bool  { return strings.HasPrefix(service, "/torchwood.client.v1.") }
func isConsoleFace(service string) bool { return strings.HasPrefix(service, "/torchwood.console.v1.") }
func isServerFace(service string) bool  { return strings.HasPrefix(service, "/torchwood.server.v1.") }
func isProjectsService(service string) bool {
	return strings.HasPrefix(service, "/torchwood.server.v1.ProjectsService")
}
