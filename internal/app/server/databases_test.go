package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestValidateAttributeType(t *testing.T) {
	d := &Databases{}
	for _, typ := range []string{"string", "INTEGER", "json"} {
		require.NoError(t, d.ValidateAttributeType(typ))
	}
	require.Error(t, d.ValidateAttributeType(""))
	st, _ := status.FromError(d.ValidateAttributeType("map"))
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestValidateIndex(t *testing.T) {
	d := &Databases{}
	require.NoError(t, d.ValidateIndex(databases.Index{
		ID:         "idx_email",
		Type:       "unique",
		Attributes: []string{"email"},
	}))
	require.Error(t, d.ValidateIndex(databases.Index{ID: "idx", Type: "unique"}))
	st, _ := status.FromError(d.ValidateIndex(databases.Index{ID: "idx", Type: "bad", Attributes: []string{"email"}}))
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestValidateIdentifier(t *testing.T) {
	d := &Databases{}
	require.NoError(t, d.ValidateIdentifier("users"))
	require.Error(t, d.ValidateIdentifier(""))
}

// TestValidateIdentifier_LengthLimit：标识符长度上限（POC 期封死 PG 63 字节截断：
// 两个仅超长部分不同的集合/属性曾会映射同一物理表/列）。
func TestValidateIdentifier_LengthLimit(t *testing.T) {
	d := &Databases{}
	require.NoError(t, d.ValidateIdentifier(strings.Repeat("a", 63)))
	st, _ := status.FromError(d.ValidateIdentifier(strings.Repeat("a", 64)))
	require.Equal(t, codes.InvalidArgument, st.Code())

	// collectionID 专用上限 40。
	require.NoError(t, d.validateCollectionID(strings.Repeat("c", 40)))
	st, _ = status.FromError(d.validateCollectionID(strings.Repeat("c", 41)))
	require.Equal(t, codes.InvalidArgument, st.Code())

	// collectionID 小写收紧（2026-09-06 勘误：物理表名 = collectionID，
	// 小写使 psql/pg_dump 等运维路径免引号直用）。
	require.NoError(t, d.validateCollectionID("posts"))
	require.NoError(t, d.validateCollectionID("_system_events"))
	require.Error(t, d.validateCollectionID("Posts"))
	require.Error(t, d.validateCollectionID("1posts"))
	require.Error(t, d.validateCollectionID("posts-app"))
	require.Error(t, d.validateCollectionID(""))

	// 索引 ID 专用上限 40。
	require.NoError(t, d.ValidateIndex(databases.Index{ID: strings.Repeat("i", 40), Type: "key", Attributes: []string{"email"}}))
	st, _ = status.FromError(d.ValidateIndex(databases.Index{ID: strings.Repeat("i", 41), Type: "key", Attributes: []string{"email"}}))
	require.Equal(t, codes.InvalidArgument, st.Code())

	// 组合校验：coll 40 + idx 20 拼接 idx_<coll>_<idx> = 65 > 63，各段合法也必须拒绝。
	require.NoError(t, validateIndexNameLen(strings.Repeat("c", 30), strings.Repeat("i", 20)))
	st, _ = status.FromError(validateIndexNameLen(strings.Repeat("c", 40), strings.Repeat("i", 20)))
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestCreateDatabase_InvalidID(t *testing.T) {
	d := &Databases{}
	for _, id := range []string{"bad-id", "bad id", "1starts_with_digit", "MyApp", "my_app"} {
		st, _ := status.FromError(d.CreateDatabase(platformAdminCtx(context.Background()), "proj", id, "name"))
		require.Equal(t, codes.InvalidArgument, st.Code(), "id %q should be rejected", id)
	}
}

// TestSeedDocumentPermissions_PerKeyRole（B14 ②）：空 ACE 种子按主体身份收敛——
// API key 主体绑 key:<自身id>（per-key 私有，取代共享 keys），user 主体绑
// owner 角色，合成 keys 主体（无 KeyID，历史形态）维持绑 keys，无角色主体
// 退化 __private__。种子是系统推导、非授予意图，execute-tx 的 R1 per-op
// 豁免结构不受种子值影响。
func TestSeedDocumentPermissions_PerKeyRole(t *testing.T) {
	requireSeedPerms(t, seedDocumentPermissions(databases.Principal{
		Roles: []string{"keys", "key:ka"}, KeyID: "ka",
	}), "key:ka", "API key 主体种子绑自身 key:<id>（B14 per-key 私有）")

	requireSeedPerms(t, seedDocumentPermissions(databases.Principal{
		Roles: []string{"users", "user:u1"},
	}), "user:u1", "user 主体种子绑 owner 角色（不变）")

	requireSeedPerms(t, seedDocumentPermissions(databases.Principal{
		Roles: []string{"keys"},
	}), "keys", "无 KeyID 的合成 keys 主体维持历史种子（测试/内部路径）")

	// owner user 角色优先于其他常规角色（admin 主体经 DocPrincipal 注入 user:<id>）。
	requireSeedPerms(t, seedDocumentPermissions(databases.Principal{
		Roles: []string{"admin", "user:a1"},
	}), "user:a1", "owner user 角色优先")

	// 无任何可绑定角色：纯私有标记。
	perms := seedDocumentPermissions(databases.Principal{})
	require.Equal(t, []databases.Permission{{Type: "read", Role: "__private__"}}, perms)
}

func requireSeedPerms(t *testing.T, perms []databases.Permission, role, msg string) {
	t.Helper()
	require.Equal(t, []databases.Permission{
		{Type: "read", Role: role},
		{Type: "update", Role: role},
		{Type: "delete", Role: role},
	}, perms, msg)
}
