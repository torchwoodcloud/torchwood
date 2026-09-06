package shared

import (
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/pkg/idgen"
)

type ActorKind string

const (
	ActorKindEndUser ActorKind = "end_user"
	ActorKindAdmin   ActorKind = "admin"
	ActorKindService ActorKind = "service" // 项目 API key
	ActorKindSystem  ActorKind = "system"
)

func (k ActorKind) IsValid() bool {
	switch k {
	case ActorKindEndUser, ActorKindAdmin, ActorKindService, ActorKindSystem:
		return true
	}
	return false
}

type CredentialType string

const (
	CredentialTypeToken   CredentialType = "token"
	CredentialTypeSession CredentialType = "session"
	CredentialTypeAPIKey  CredentialType = "api_key"
)

const (
	ConsoleSessionCookieName = "TORCHWOOD_session_console"
	SessionCookiePrefix      = "TORCHWOOD_session_"
	// RoleConsole 是 proto ACCESS_PERMISSION 标签，只给拦截器 HasAnyRole 用，
	// 不得进入文档 ACL（DocPrincipal 会剔除）。
	RoleConsole = "console"
)

type Principal struct {
	ActorID         idgen.ID
	ActorKind       ActorKind
	CredentialType  CredentialType
	IsPlatformAdmin bool
	ProjectID       string
	UserID          string // 仅 EndUser
	AdminID         string // 仅 Admin；禁止把 admin id 塞进 UserID
	APIKeyID        string // 仅 Service
	SessionID       string
	Roles           []string // 文档 ACL / console RBAC，不是 API scope
	Permissions     []string // API key scopes
	Email           string
}

// NewSystemPrincipal 构造内部 System 主体（worker / 履约），不是缺字段的 API key。
func NewSystemPrincipal(projectID string) *Principal {
	return &Principal{
		ActorID:   idgen.ID("system"),
		ActorKind: ActorKindSystem,
		ProjectID: projectID,
	}
}

func (p *Principal) IsSystem() bool {
	return p != nil && p.ActorKind == ActorKindSystem
}

func (p *Principal) IsAuthenticated() bool {
	if p == nil {
		return false
	}
	switch p.ActorKind {
	case ActorKindEndUser:
		return p.UserID != ""
	case ActorKindAdmin:
		return p.AdminID != ""
	case ActorKindService:
		return p.APIKeyID != ""
	case ActorKindSystem:
		return true
	default:
		return false
	}
}

// OwnerID 是文件/上传会话的属主：端用户用 UserID，admin 用 AdminID。
// Service 无属主（空串），不要把 admin id 塞回 UserID。
func (p *Principal) OwnerID() string {
	if p == nil {
		return ""
	}
	switch p.ActorKind {
	case ActorKindEndUser:
		return p.UserID
	case ActorKindAdmin:
		return p.AdminID
	default:
		return ""
	}
}

// AdminLookupID 取 admin 查找键：优先 AdminID，测试桩可回退 ActorID。
func (p *Principal) AdminLookupID() string {
	if p == nil || p.ActorKind != ActorKindAdmin {
		return ""
	}
	if p.AdminID != "" {
		return p.AdminID
	}
	return string(p.ActorID)
}

func (p *Principal) HasRole(role string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func (p *Principal) HasScope(scope string) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Permissions {
		if s == scope {
			return true
		}
	}
	return false
}

// HasAnyRole 报告是否命中任一角色。空列表 fail-closed（false）；
// proto ACCESS_PERMISSION 的空列表由启动期 collectMethodsByAccess 守门。
func (p *Principal) HasAnyRole(roles []string) bool {
	if p == nil || len(roles) == 0 {
		return false
	}
	for _, role := range roles {
		if p.HasRole(role) {
			return true
		}
	}
	return false
}

// DocPrincipal 投影到文档 ACL 视图。System 走 databases.SystemPrincipal。
// 投影净化（M6.3，机制重设计）：console 命名空间整组出局——会话标签
// RoleConsole 与 admin RBAC 裸角色（owner/admin/member/viewer）都不是文档
// 角色；admin 的文档身份 = user:<AdminID>（属主匹配）+ PlatformAdmin flag
// （bypass 走 flag，不走角色串）。API key 主体带 KeyID 供写入归因。
func (p *Principal) DocPrincipal() databases.Principal {
	if p == nil {
		return databases.GuestPrincipal
	}
	if p.IsSystem() {
		return databases.SystemPrincipal
	}
	roles := make([]string, 0, len(p.Roles)+1)
	for _, r := range p.Roles {
		if isConsoleNamespaceRole(r) {
			continue
		}
		roles = append(roles, r)
	}
	if p.ActorKind == ActorKindAdmin && p.AdminID != "" {
		owner := "user:" + p.AdminID
		found := false
		for _, r := range roles {
			if r == owner {
				found = true
				break
			}
		}
		if !found {
			roles = append(roles, owner)
		}
	}
	keyID := ""
	if p.ActorKind == ActorKindService {
		keyID = p.APIKeyID
	}
	return databases.Principal{Roles: roles, PlatformAdmin: p.IsPlatformAdmin, KeyID: keyID}
}

// consoleNamespaceRoles 是 console RBAC 命名空间（平台级角色词表，M6.3
// 投影互斥不变量）：这些串不携带文档语义，进入文档角色集/RLS app.roles
// 只会制造撞名（DefaultCollectionPermissions 的 "admin" ACE 即历史撞名）。
var consoleNamespaceRoles = map[string]struct{}{
	RoleConsole: {},
	"owner":     {},
	"admin":     {},
	"member":    {},
	"viewer":    {},
}

func isConsoleNamespaceRole(r string) bool {
	_, ok := consoleNamespaceRoles[r]
	return ok
}
