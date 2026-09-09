package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCreateAttribute_RejectsReservedVersion (PR1)：用户属性名禁止使用系统保留列
// （含 _version/_perms 等）。ValidateIdentifier 允许 "_" 前缀，必须在属性创建
// 路径显式拒绝；CreateCollection 属性列表同样校验。
func TestCreateAttribute_RejectsReservedVersion(t *testing.T) {
	uc := NewDatabases(fakeProjectRepo{}, newFakeDocDB(), nil)
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"},
		Permissions: []string{"databases.write"},
	})

	for _, key := range []string{"_id", "_tenant", "_created_at", "_updated_at", "_created_by", "_updated_by", "_version", "_perms"} {
		t.Run(key, func(t *testing.T) {
			err := uc.CreateAttribute(ctx, "proj-1", "app", "coll", databases.Attribute{
				Key:  key,
				Type: "string",
			})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Contains(t, err.Error(), fmt.Sprintf("attribute key %q is reserved", key))
		})
	}

	// CreateCollection 属性列表同样拒绝（防绕过 CreateAttribute 直调）。
	err := uc.CreateCollection(ctx, "proj-1", "app", "coll", "Coll", []databases.Attribute{
		{Key: "_version", Type: "integer"},
	}, nil, nil, false)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), `attribute key "_version" is reserved`)
}
