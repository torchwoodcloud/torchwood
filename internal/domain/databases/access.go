package databases

import "strings"

// Principal is the access context for document operations.
// It is constructed by the app layer from shared.Principal and passed to
// DocumentDB implementations. Keeping it in domain/databases avoids a
// dependency on internal/domain/shared from the document port.
type Principal struct {
	// Roles carries the caller's role strings（形态由 docrole.go 词表定义，
	// 如 RoleUsers / RoleUser(id) / RoleGroup(id) / RoleKeys / RoleAny）。
	// RoleSystem 是 ActorKind System 的文档投影，bypass 文档级检查。
	Roles []string

	// PlatformAdmin indicates the caller is a console admin with full
	// access (bypasses document-level permission checks). 这不是 System actor.
	PlatformAdmin bool

	// KeyID 是 API key 主体（ActorKind=Service）的 key ID，用于写入归因：
	// _created_by/_updated_by 落 "key:<id>"（redesign §10.2-1——一等 Agent
	// 的最低要求是行为可归因）。非 key 主体为空。
	KeyID string
}

// GuestPrincipal is used for unauthenticated Client API read requests.
var GuestPrincipal = Principal{Roles: []string{RoleGuests}}

// SystemPrincipal is the principal used by internal infrastructure paths
// (session validation, post-create reads, email lookup). It bypasses all
// document-level permission checks.
var SystemPrincipal = Principal{
	Roles: []string{RoleSystem},
}

// HasRole reports whether the principal holds the given role.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// IsSystem reports whether this is the internal System actor projection
// （RoleSystem 角色），不是 PlatformAdmin。
func (p Principal) IsSystem() bool {
	return p.HasRole(RoleSystem)
}

// BypassesDocumentACL reports whether document permission checks should be
// skipped. System（内部）与 PlatformAdmin（console）都旁路，但它们不是同一 Actor.
func (p Principal) BypassesDocumentACL() bool {
	return p.IsSystem() || p.PlatformAdmin
}

// StableActorID 返回主体的稳定归因身份（写幂等键作用域用，redesign §10.1——
// 复用 _created_by 归因链）：首个 user:<id> 角色 → 裸 id（end user / console
// admin 经 DocPrincipal 注入 user:<AdminID>）；key 主体 → key:<id>；内部
// System → "system"。返回空串表示无稳定身份（理论不可达），调用方跳过幂等
// 而不是让所有匿名主体共享同一命名空间。
func (p Principal) StableActorID() string {
	for _, r := range p.Roles {
		if id, ok := strings.CutPrefix(r, RolePrefixUser); ok && id != "" {
			return id
		}
	}
	if p.KeyID != "" {
		return RoleKey(p.KeyID)
	}
	if p.IsSystem() {
		return "system"
	}
	return ""
}
