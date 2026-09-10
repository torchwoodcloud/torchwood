package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCreateAttribute_ArrayElementWhitelist（阶段③-b 预决策 1，D-5 修订）：
// array=true 已是合法属性（PG 原生 T[] 列），但元素类型仅限标量子集——
// email/url/json 数组拒绝；CreateCollection 属性列表同口径。
func TestCreateAttribute_ArrayElementWhitelist(t *testing.T) {
	uc := NewDatabases(fakeProjectRepo{}, newFakeDocDB(), nil)
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"},
		Permissions: []string{"databases.write"},
	})

	for _, elemType := range []string{"email", "url", "json"} {
		err := uc.CreateAttribute(ctx, "proj-1", "app", "coll", databases.Attribute{
			Key: "tags", Type: elemType, Array: true,
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "type=%s", elemType)
		require.Contains(t, err.Error(), "array supports string, integer, float, boolean, datetime element types")
	}

	err := uc.CreateCollection(ctx, "proj-1", "app", "coll", "Coll", []databases.Attribute{
		{Key: "title", Type: "string"},
		{Key: "links", Type: "url", Array: true},
	}, nil, nil, false)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), `attribute "links": array supports`)

	// 合法元素类型（五类）全通过 app 层校验（物理落库由集成测试覆盖）。
	for _, elemType := range []string{"string", "integer", "float", "boolean", "datetime"} {
		require.NoError(t, uc.CreateAttribute(ctx, "proj-1", "app", "coll", databases.Attribute{
			Key: "tags", Type: elemType, Array: true,
		}), "type=%s", elemType)
	}
	require.NoError(t, uc.CreateCollection(ctx, "proj-1", "app", "coll", "Coll", []databases.Attribute{
		{Key: "title", Type: "string"},
		{Key: "tags", Type: "string", Array: true},
	}, nil, nil, false))

	require.NoError(t, uc.CreateAttribute(ctx, "proj-1", "app", "coll", databases.Attribute{
		Key: "title", Type: "string",
	}))
}
