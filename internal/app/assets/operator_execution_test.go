package assets

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	domainassets "github.com/torchwooddev/torchwood/internal/domain/assets"
	domainshared "github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
)

// P0 执行身份：execution principal 的账本 operator 快照映射——actor_kind=
// execution、actor_id=function_id、user_id=触发用户（Server 面触发为空）。
func TestOperatorFrom_ExecutionPrincipal(t *testing.T) {
	ctx := contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorKind:      domainshared.ActorKindExecution,
		CredentialType: domainshared.CredentialTypeExecution,
		ProjectID:      "p1",
		FunctionID:     "fn_1",
		ExecutionID:    "e1",
		InvokingUserID: "u1",
	})

	var snap domainassets.OperatorSnapshot
	raw := operatorFrom(ctx)
	require.NoError(t, json.Unmarshal(raw, &snap))
	require.Equal(t, "execution", snap.ActorKind)
	require.Equal(t, "fn_1", snap.ActorID)
	require.Equal(t, "u1", snap.UserID)
	require.Equal(t, "execution", snap.CredentialType)
	require.False(t, snap.IsSystem)
	require.Empty(t, snap.APIKeyID, "execution 不冒用 api_key_id 字段")
}

// Server 面触发（无 InvokingUserID）：user_id 如实留空（omitempty）。
func TestOperatorFrom_ExecutionPrincipalWithoutInvokingUser(t *testing.T) {
	ctx := contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorKind:      domainshared.ActorKindExecution,
		CredentialType: domainshared.CredentialTypeExecution,
		ProjectID:      "p1",
		FunctionID:     "fn_1",
		ExecutionID:    "e1",
	})

	var snap domainassets.OperatorSnapshot
	raw := operatorFrom(ctx)
	require.NoError(t, json.Unmarshal(raw, &snap))
	require.Equal(t, "execution", snap.ActorKind)
	require.Equal(t, "fn_1", snap.ActorID)
	require.Empty(t, snap.UserID)
}

// 既有主体映射不变（回归锚点：API key 走 api_key_id，system 保持 is_system）。
func TestOperatorFrom_ExistingPrincipalsUnchanged(t *testing.T) {
	keyCtx := contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorKind:      domainshared.ActorKindService,
		CredentialType: domainshared.CredentialTypeAPIKey,
		APIKeyID:       "k1",
	})
	var snap domainassets.OperatorSnapshot
	require.NoError(t, json.Unmarshal(operatorFrom(keyCtx), &snap))
	require.Equal(t, "service", snap.ActorKind)
	require.Equal(t, "k1", snap.APIKeyID)

	var sysSnap domainassets.OperatorSnapshot
	require.NoError(t, json.Unmarshal(operatorFrom(context.Background()), &sysSnap))
	require.True(t, sysSnap.IsSystem)

	var anon domainassets.OperatorSnapshot
	require.NoError(t, json.Unmarshal(operatorFrom(contexts.WithPrincipal(context.Background(), &domainshared.Principal{})), &anon))
	require.False(t, anon.IsSystem)
}
