package shared

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
)

func TestActorKind_AgentForbidden(t *testing.T) {
	t.Parallel()
	require.False(t, ActorKind("agent").IsValid())
	require.True(t, ActorKindSystem.IsValid())
	require.True(t, ActorKindService.IsValid())
}

func TestPrincipal_IsAuthenticated_ByKind(t *testing.T) {
	t.Parallel()
	require.False(t, (*Principal)(nil).IsAuthenticated())
	require.False(t, (&Principal{ActorKind: ActorKindEndUser}).IsAuthenticated())
	require.True(t, (&Principal{ActorKind: ActorKindEndUser, UserID: "u1"}).IsAuthenticated())
	require.False(t, (&Principal{ActorKind: ActorKindAdmin, UserID: "admin-1"}).IsAuthenticated(), "admin id 不得塞进 UserID")
	require.False(t, (&Principal{ActorKind: ActorKindAdmin, ActorID: "admin-1"}).IsAuthenticated(), "仅 ActorID 不算已认证 admin")
	require.True(t, (&Principal{ActorKind: ActorKindAdmin, AdminID: "admin-1"}).IsAuthenticated())
	require.True(t, (&Principal{ActorKind: ActorKindService, APIKeyID: "k1"}).IsAuthenticated())
	require.False(t, (&Principal{ActorKind: ActorKindService}).IsAuthenticated())
	require.True(t, NewSystemPrincipal("p1").IsAuthenticated())
}

func TestPrincipal_IsSystem(t *testing.T) {
	t.Parallel()
	sys := NewSystemPrincipal("p1")
	require.True(t, sys.IsSystem())
	require.Equal(t, ActorKindSystem, sys.ActorKind)
	require.Empty(t, sys.APIKeyID)
	require.NotEqual(t, CredentialTypeAPIKey, sys.CredentialType)
	require.False(t, (&Principal{ActorKind: ActorKindService, APIKeyID: ""}).IsSystem())
	require.Equal(t, databases.SystemPrincipal, sys.DocPrincipal())
}

func TestPrincipal_HasAnyRole_FailClosed(t *testing.T) {
	t.Parallel()
	p := &Principal{Roles: []string{"users"}, Permissions: []string{"storage.read"}}
	require.True(t, p.HasAnyRole([]string{"users"}))
	require.False(t, p.HasAnyRole([]string{"storage.read"}))
	require.False(t, p.HasAnyRole(nil))
	require.True(t, p.HasScope("storage.read"))
	require.False(t, p.HasScope("users"))
}

func TestPrincipal_DocPrincipal_PlatformAdmin(t *testing.T) {
	t.Parallel()
	p := &Principal{Roles: []string{"users"}, IsPlatformAdmin: true}
	got := p.DocPrincipal()
	require.Equal(t, []string{"users"}, got.Roles)
	require.True(t, got.PlatformAdmin)
	require.False(t, got.IsSystem())
	require.True(t, got.BypassesDocumentACL())
}

func TestPrincipal_OwnerID(t *testing.T) {
	t.Parallel()
	require.Equal(t, "u1", (&Principal{ActorKind: ActorKindEndUser, UserID: "u1"}).OwnerID())
	require.Equal(t, "a1", (&Principal{ActorKind: ActorKindAdmin, AdminID: "a1", UserID: "should-not"}).OwnerID())
	require.Empty(t, (&Principal{ActorKind: ActorKindService, APIKeyID: "k1"}).OwnerID())
	require.Equal(t, "a1", (&Principal{ActorKind: ActorKindAdmin, AdminID: "a1", ActorID: "other"}).AdminLookupID())
	require.Equal(t, "legacy", (&Principal{ActorKind: ActorKindAdmin, ActorID: "legacy"}).AdminLookupID())
}

func TestPrincipal_DocPrincipal_DropsConsoleTag(t *testing.T) {
	t.Parallel()
	p := &Principal{
		ActorKind: ActorKindAdmin,
		AdminID:   "a1",
		Roles:     []string{"member", RoleConsole},
	}
	got := p.DocPrincipal()
	// 投影净化（M6.3 决策 v8）：console 命名空间整组出局（含 RBAC 裸角色
	// member），admin 的文档身份 = user:<AdminID> + PlatformAdmin flag。
	require.NotContains(t, got.Roles, RoleConsole)
	require.NotContains(t, got.Roles, "member")
	require.Contains(t, got.Roles, "user:a1")
	require.True(t, p.HasAnyRole([]string{RoleConsole}))
}

// TestPrincipal_DocPrincipal_KeyAttribution：API key 主体投影携带 KeyID
// （写入归因链路：_created_by/_updated_by 落 key:<id>）；非 key 主体不携带。
func TestPrincipal_DocPrincipal_KeyAttribution(t *testing.T) {
	t.Parallel()

	svc := &Principal{ActorKind: ActorKindService, APIKeyID: "k123", Roles: []string{"keys"}}
	got := svc.DocPrincipal()
	require.Equal(t, "k123", got.KeyID)

	user := &Principal{ActorKind: ActorKindEndUser, UserID: "u1", Roles: []string{"users"}}
	require.Empty(t, user.DocPrincipal().KeyID)

	// key 角色与 APIKeyID 分属两层：Roles 携带 keys（ACL 匹配），KeyID 携带
	// 具体实例（归因）。
	admin := &Principal{ActorKind: ActorKindAdmin, AdminID: "a1"}
	require.Empty(t, admin.DocPrincipal().KeyID)
}
