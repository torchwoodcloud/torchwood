package databases

import "strings"

// DocRole 词表（M6.1 主权声明，机制 v8）：文档 ACE / 集合权限可引用的角色
// 字符串唯一构造与解析来源。域层纯函数，无 genproto / infra 依赖；生产代码
// 禁止裸串拼接文档角色（由 docrole_test.go 的裸串扫描测试守门）。
//
// 词表形态：
//   - 裸词表项：users / any / guests（Appwrite 语义角色）
//   - 命名空间角色：user:<id>[/verified]、group:<gid>[/<role>]、key:<id>、
//     member:<mid>、label:<label>（冒号前缀 + 身份段，可带复合后缀）
//   - sentinel：__private__（纯私有占位 ACE）、__system__（内部旁路投影）
//   - 模板占位符：user:{id} / group:{id}（Appwrite 授予模板，持久化前展开）
const (
	// 裸词表项（无命名空间前缀）。
	RoleUsers  = "users"  // 已认证端用户（JWT 角色解析恒含，见 authz_policy END_USER 归一）
	RoleAny    = "any"    // 公开（含匿名；写类授予被 syntheticRoles 拒绝）
	RoleGuests = "guests" // GuestPrincipal 唯一角色
	RoleKeys   = "keys"   // API key scope 面（集合默认权限、特权授予判定）

	// sentinel（双下划线包裹，不与任何命名空间撞形）。
	RolePrivate = "__private__" // 纯私有标记 ACE：无常规角色可绑定的占位
	RoleSystem  = "__system__"  // 内部基础设施旁路（SystemPrincipal / SystemRoles）

	// 模板占位符（授予侧，ExpandPermissionTemplates 展开）。
	RoleUserTemplate  = "user:{id}"
	RoleGroupTemplate = "group:{id}"
)

// 命名空间前缀与复合后缀（构造器原料，前缀匹配方亦应引用而非裸串）。
const (
	RolePrefixUser         = "user:"
	RolePrefixGroup        = "group:"
	RolePrefixKey          = "key:"
	RolePrefixMember       = "member:"
	RolePrefixLabel        = "label:"
	RoleVerifiedTag        = "verified"
	RoleVerifiedSuffix     = "/" + RoleVerifiedTag
	RoleGroupRoleSeparator = "/"
)

// ParseDocRole 返回的角色类别（kind）。
const (
	RoleKindUser    = "user"
	RoleKindGroup   = "group"
	RoleKindKey     = "key"
	RoleKindMember  = "member"
	RoleKindLabel   = "label"
	RoleKindUsers   = "users"
	RoleKindAny     = "any"
	RoleKindGuests  = "guests"
	RoleKindPrivate = "private"
	RoleKindSystem  = "system"
)

// RoleKey 构造 API key 数据隔离身份角色（key:<id>）：RLS 谓词可见、可作文档
// ACE 授予目标（B14 per-key 角色）。
func RoleKey(id string) string { return RolePrefixKey + id }

// RoleUser 构造端用户/管理员的文档身份角色（user:<id>）。
func RoleUser(id string) string { return RolePrefixUser + id }

// RoleUserVerified 构造邮箱已验证复合角色（user:<id>/verified）。
func RoleUserVerified(id string) string { return RoleUser(id) + RoleVerifiedSuffix }

// RoleGroup 构造组成员身份角色（group:<gid>）。
func RoleGroup(gid string) string { return RolePrefixGroup + gid }

// RoleGroupRole 构造组内职务复合角色（group:<gid>/<role>）。组域裸角色
// （owner/admin/member，见 domain/groups）只有经本构造器复合后才进入文档
// 角色集，避免与 console RBAC 角色撞名（M6.3 投影互斥不变量）。
func RoleGroupRole(gid, role string) string { return RoleGroup(gid) + RoleGroupRoleSeparator + role }

// RoleMembership 构造成员关系身份角色（member:<mid>）。
func RoleMembership(mid string) string { return RolePrefixMember + mid }

// RoleLabel 构造用户标签角色（label:<label>）。
func RoleLabel(label string) string { return RolePrefixLabel + label }

// ParseDocRole 将 s 解析为 (kind, id, extra, ok)。ok=false 表示不在词表内
// （含空身份段、未知复合后缀与一切裸串，如组域裸角色 owner/admin/member）。
//
//	extra 仅复合形态非空：user:<id>/verified → extra="verified"；
//	group:<gid>/<role> → extra=<role>。user 域仅认 verified 后缀。
func ParseDocRole(s string) (kind, id, extra string, ok bool) {
	switch s {
	case RoleUsers:
		return RoleKindUsers, "", "", true
	case RoleAny:
		return RoleKindAny, "", "", true
	case RoleGuests:
		return RoleKindGuests, "", "", true
	case RolePrivate:
		return RoleKindPrivate, "", "", true
	case RoleSystem:
		return RoleKindSystem, "", "", true
	}
	if rest, found := strings.CutPrefix(s, RolePrefixUser); found {
		main, suffix, hasSuffix := strings.Cut(rest, RoleGroupRoleSeparator)
		if main == "" {
			return "", "", "", false
		}
		if !hasSuffix {
			return RoleKindUser, main, "", true
		}
		if suffix != RoleVerifiedTag {
			return "", "", "", false
		}
		return RoleKindUser, main, suffix, true
	}
	if rest, found := strings.CutPrefix(s, RolePrefixGroup); found {
		main, extra, hasExtra := strings.Cut(rest, RoleGroupRoleSeparator)
		if main == "" || (hasExtra && extra == "") {
			return "", "", "", false
		}
		return RoleKindGroup, main, extra, true
	}
	if rest, found := strings.CutPrefix(s, RolePrefixKey); found && rest != "" {
		return RoleKindKey, rest, "", true
	}
	if rest, found := strings.CutPrefix(s, RolePrefixMember); found && rest != "" {
		return RoleKindMember, rest, "", true
	}
	if rest, found := strings.CutPrefix(s, RolePrefixLabel); found && rest != "" {
		return RoleKindLabel, rest, "", true
	}
	return "", "", "", false
}

// IsGroupRoleFormat 报告 s 是否为 group:<gid>/<role> 复合格式。组域裸角色
// （"owner"/"admin"/"member"）一律 false——组角色只有复合形态才可进入文档
// 角色集。
func IsGroupRoleFormat(s string) bool {
	kind, _, extra, ok := ParseDocRole(s)
	return ok && kind == RoleKindGroup && extra != ""
}

// IsNamespacedRole 报告 s 是否为带前缀的命名空间角色（user:/group:/key:/
// member:/label:），与裸词表项（users/any/guests）、sentinel
// （__private__/__system__）相对。
func IsNamespacedRole(s string) bool {
	kind, _, _, ok := ParseDocRole(s)
	switch kind {
	case RoleKindUser, RoleKindGroup, RoleKindKey, RoleKindMember, RoleKindLabel:
		return ok
	default:
		return false
	}
}
