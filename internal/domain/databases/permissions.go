package databases

import (
	"fmt"
	"strings"
)

// SystemRoles bypass document-level _perms checks. Use only for internal
// infrastructure paths (session validation, post-create reads, email lookup).
var SystemRoles = []string{"__system__"}

// DefaultCollectionPermissions returns a reasonable default permission set for
// user-created collections that do not specify explicit permissions.
// WHY: 默认集合不再含 read:any，避免空权限文档通过集合回落对 guest 可读；公开集合需显式授予 read:any。
// admin ACE 已删除（M6.3 决策 v8）：唯一能持裸 "admin" 的主体（console
// role=admin）必为 PlatformAdmin 走 bypass，该 ACE 是死语义且与 console
// 角色撞名；DocPrincipal 投影净化后裸 console 角色串不再进入文档角色集。
func DefaultCollectionPermissions() []Permission {
	return []Permission{
		{Type: "create", Role: "users"},
		{Type: "update", Role: "users"},
		{Type: "delete", Role: "users"},
		{Type: "create", Role: "keys"},
		{Type: "read", Role: "keys"},
		{Type: "update", Role: "keys"},
		{Type: "delete", Role: "keys"},
	}
}

// ExpandPermissionRoles augments caller roles for ACL matching.
// "any" is always included (public read:any). "users" is added only when the
// caller is authenticated (has the users role).
func ExpandPermissionRoles(roles []string) []string {
	seen := make(map[string]struct{}, len(roles)+2)
	out := make([]string, 0, len(roles)+2)
	hasUsers := false
	for _, r := range roles {
		if r == "users" {
			hasUsers = true
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	if _, ok := seen["any"]; !ok {
		out = append(out, "any")
	}
	if hasUsers {
		if _, ok := seen["users"]; !ok {
			out = append(out, "users")
		}
	}
	return out
}

// syntheticRoles 由 ExpandPermissionRoles 无条件注入、不构成授予凭据的角色。
// "any" 的写类授予一律拒绝；read 类授予保留（文档公开读取是集合级显式行为）。
var syntheticRoles = map[string]struct{}{
	"any": {},
}

// CollectionAllows reports whether the collection-level permission list grants
// the given operation type to any of the provided roles.
// "write" on a permission expands to create, update, and delete.
func CollectionAllows(perms []Permission, permType string, roles []string) bool {
	types := matchTypes(permType)
	for _, p := range perms {
		if !containsType(types, p.Type) {
			continue
		}
		for _, r := range roles {
			if p.Role == r {
				return true
			}
		}
	}
	return false
}

// AllowsDocumentAccess implements Appwrite-style documentSecurity semantics (B1):
//   - documentSecurity=false: only collection permissions apply
//   - documentSecurity=true:
//   - document has no _perms rows (docHasPerms=false): collection permissions apply
//   - document has _perms rows:
//   - system collections (D1 豁免): collection OR document permissions
//   - user collections: document permissions override collection permissions
func AllowsDocumentAccess(coll *Collection, docPerms []Permission, docHasPerms bool, permType string, roles []string) bool {
	if coll == nil {
		return false
	}
	expanded := ExpandPermissionRoles(roles)
	collOK := CollectionAllows(coll.Permissions, permType, expanded)
	if !coll.DocumentSecurity {
		return collOK
	}
	if !docHasPerms {
		return collOK
	}
	if coll.IsSystem {
		// D1 豁免：系统集合保持 OR 语义，保证显式 permissions（不含 read:any）的
		// 系统集合文档仍由集合级兜底（匿名读 groups/buckets 依赖此行为）。
		return collOK || CollectionAllows(docPerms, permType, expanded)
	}
	// 用户集合：文档权限覆盖集合权限（Appwrite 语义，"私有文档"生效）。
	return CollectionAllows(docPerms, permType, expanded)
}

// ListAccessDenied reports whether list/count should be rejected outright.
func ListAccessDenied(coll *Collection, roles []string) bool {
	if coll == nil {
		return true
	}
	expanded := ExpandPermissionRoles(roles)
	if CollectionAllows(coll.Permissions, "read", expanded) {
		return false
	}
	return !coll.DocumentSecurity
}

// SkipDocumentPermissionFilter reports whether list/count can skip per-document
// permission SQL. 仅当（系统集合且集合级有 read）或（!DocumentSecurity 且集合级
// 有 read）时跳过；用户集合 documentSecurity=true 一律逐文档过滤（B1）。
func SkipDocumentPermissionFilter(coll *Collection, roles []string) bool {
	if coll == nil {
		return false
	}
	collRead := CollectionAllows(coll.Permissions, "read", ExpandPermissionRoles(roles))
	if !collRead {
		return false
	}
	if coll.IsSystem {
		return true // D1 豁免
	}
	return !coll.DocumentSecurity
}

// FormatPermissionString renders a permission as type:role.
func FormatPermissionString(p Permission) string {
	return p.Type + ":" + p.Role
}

// ParsePermissionStrings converts "read:any" style strings into Permission values.
// "write:role" expands to create, update, and delete for that role.
func ParsePermissionStrings(items []string) ([]Permission, error) {
	if len(items) == 0 {
		return DefaultCollectionPermissions(), nil
	}
	out := make([]Permission, 0, len(items))
	for _, item := range items {
		typ, role, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok || typ == "" || role == "" {
			return nil, fmt.Errorf("invalid permission %q (expected type:role)", item)
		}
		if typ == "write" {
			out = append(out,
				Permission{Type: "create", Role: role},
				Permission{Type: "update", Role: role},
				Permission{Type: "delete", Role: role},
			)
			continue
		}
		out = append(out, Permission{Type: typ, Role: role})
	}
	return out, nil
}

// ExpandPermissionTemplates replaces the Appwrite-style placeholders
// "user:{id}" and "group:{id}" in permission roles with the caller's first
// matching concrete role (e.g. "user:<uuid>"), preserving original entries
// when no matching role is held. The expanded set is used for grant
// validation and persistence, mirroring Appwrite's create/update semantics.
func ExpandPermissionTemplates(perms []Permission, roles []string) []Permission {
	if len(perms) == 0 {
		return perms
	}
	firstUser, firstGroup := firstPrefixedRole(roles, "user:"), firstPrefixedRole(roles, "group:")
	if firstUser == "" && firstGroup == "" {
		return perms
	}
	out := make([]Permission, len(perms))
	for i, p := range perms {
		switch p.Role {
		case "user:{id}":
			if firstUser != "" {
				out[i] = Permission{Type: p.Type, Role: firstUser}
				continue
			}
		case "group:{id}":
			if firstGroup != "" {
				out[i] = Permission{Type: p.Type, Role: firstGroup}
				continue
			}
		}
		out[i] = p
	}
	return out
}

// firstPrefixedRole returns the first role with the given prefix, without the
// prefix, or "" when none is held.
func firstPrefixedRole(roles []string, prefix string) string {
	for _, r := range roles {
		if strings.HasPrefix(r, prefix) {
			return r
		}
	}
	return ""
}

// ValidateGrantablePermissions ensures the grantor may assign the given roles.
// Privileged callers (API key via keys role with scopes, platform admin) skip checks.
func ValidateGrantablePermissions(grantor Principal, perms []Permission, privileged bool) error {
	if privileged || grantor.BypassesDocumentACL() {
		return nil
	}
	expanded := ExpandPermissionRoles(grantor.Roles)
	for _, p := range perms {
		if p.Type == "create" {
			continue
		}
		if _, synthetic := syntheticRoles[p.Role]; synthetic {
			if p.Type != "read" {
				return fmt.Errorf("cannot grant %q for role %q", p.Type, p.Role)
			}
			continue
		}
		if !roleHeld(expanded, p.Role) {
			return fmt.Errorf("cannot grant role %q without holding it", p.Role)
		}
	}
	return nil
}

func roleHeld(roles []string, target string) bool {
	for _, r := range roles {
		if r == target {
			return true
		}
	}
	return false
}

func matchTypes(permType string) []string {
	switch permType {
	case "create", "update", "delete":
		return []string{permType, "write"}
	default:
		return []string{permType}
	}
}

func containsType(types []string, t string) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}
