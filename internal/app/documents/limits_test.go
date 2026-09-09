package documents

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// nPerms 构造 n 条互不重复的 ACE（边界/超限用例的 permissions 参数）。
func nPerms(n int) []databases.Permission {
	perms := make([]databases.Permission, 0, n)
	for i := 0; i < n; i++ {
		perms = append(perms, databases.Permission{Type: "read", Role: fmt.Sprintf("group:g%d", i)})
	}
	return perms
}

// TestACL_Limit（redesign §11-J H2）：_acl ≤64 ACE，超限 InvalidArgument /
// DOCUMENT.ACL_TOO_LARGE 且先于 adapter（create/update/upsert/bulk 四写路径
// 共用 validateACL）；边界 64 条放行。种子路径（空 ACE → ≤3 条系统推导）天然
// 合法，无需豁免。
func TestACL_Limit(t *testing.T) {
	ctx := context.Background()
	principal := databases.Principal{Roles: []string{"keys"}}
	opts := WriteOptions{AllowPrivilegedGrant: true}

	t.Run("over limit rejected before adapter", func(t *testing.T) {
		rec := newMemDocDB()
		core := New(rec, nil)
		over := nPerms(MaxDocumentACL + 1)

		_, _, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, over, principal, "", opts)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "DOCUMENT.ACL_TOO_LARGE")
		require.Zero(t, rec.creates)

		v := int64(1)
		_, _, err = core.UpdateDocument(ctx, "p", "app", "notes", "d1", nil, over, nil, nil, principal, &v, "", opts)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "DOCUMENT.ACL_TOO_LARGE")

		_, _, err = core.UpsertDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, []string{"x"}, over, principal, "", opts)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "DOCUMENT.ACL_TOO_LARGE")
		require.Zero(t, rec.upserts)

		_, _, err = core.BulkUpdateDocuments(ctx, "p", "app", "notes", []string{"d1"}, map[string]any{"t": 1}, over, principal, "", opts)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "DOCUMENT.ACL_TOO_LARGE")
		require.Zero(t, rec.bulkUpdates)
	})

	t.Run("boundary 64 entries pass", func(t *testing.T) {
		rec := newMemDocDB()
		core := New(rec, nil)
		_, _, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"t": 1}, nPerms(MaxDocumentACL), principal, "", opts)
		require.NoError(t, err)
		require.Equal(t, 1, rec.creates)
	})
}

// TestArrayElements_Limit（redesign §11-J H2）：数组 ≤1000 元素——data 通道
// （JSON 反序列化 []any 与服务端构造 []string 两形态）与 array_updates 的
// values 通道；超限 InvalidArgument / DOCUMENT.TOO_LARGE，边界 1000 放行。
func TestArrayElements_Limit(t *testing.T) {
	ctx := context.Background()
	principal := databases.Principal{Roles: []string{"keys"}}
	elems := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = "v"
		}
		return out
	}

	t.Run("data channel over limit rejected", func(t *testing.T) {
		err := ValidateDocumentPayload(map[string]any{"tags": elems(MaxArrayElements + 1)})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "DOCUMENT.TOO_LARGE")

		// []string 形态（服务端构造路径）同样受限。
		err = ValidateDocumentPayload(map[string]any{
			"labels": func() []string {
				out := make([]string, MaxArrayElements+1)
				for i := range out {
					out[i] = "v"
				}
				return out
			}(),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "DOCUMENT.TOO_LARGE")
	})

	t.Run("data channel boundary passes", func(t *testing.T) {
		require.NoError(t, ValidateDocumentPayload(map[string]any{"tags": elems(MaxArrayElements)}))
	})

	t.Run("array_updates values over limit rejected before adapter", func(t *testing.T) {
		rec := newMemDocDB()
		core := New(rec, nil)
		values := make([]string, MaxArrayElements+1)
		for i := range values {
			values[i] = "v"
		}
		v := int64(1)
		_, _, err := core.UpdateDocument(ctx, "p", "app", "notes", "d1", nil, nil, nil,
			map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: values}},
			principal, &v, "", WriteOptions{})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "DOCUMENT.TOO_LARGE")
	})

	t.Run("array_updates boundary passes validation", func(t *testing.T) {
		core := New(newMemDocDB(), nil)
		values := make([]string, MaxArrayElements)
		for i := range values {
			values[i] = "v"
		}
		v := int64(1)
		// 集合/文档不存在 → NotFound：证明已通过前置校验进入 adapter。
		_, _, err := core.UpdateDocument(ctx, "p", "app", "notes", "d1", nil, nil, nil,
			map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: values}},
			principal, &v, "", WriteOptions{})
		require.Equal(t, codes.NotFound, status.Code(err))
	})
}

// TestObjectDepth_Limit（redesign §11-J H2）：object 嵌套 ≤8 层（顶层属性
// 对象为第 1 层，数组透明不计层），超限 InvalidArgument / DOCUMENT.TOO_LARGE，
// 边界 8 层放行。
func TestObjectDepth_Limit(t *testing.T) {
	nest := func(depth int) map[string]any {
		// leaf = 第 depth 层对象；返回含它的顶层载荷。
		leaf := map[string]any{"v": 1}
		cur := leaf
		for i := 1; i < depth; i++ {
			cur = map[string]any{"n": cur}
		}
		return map[string]any{"obj": cur}
	}

	require.NoError(t, ValidateDocumentPayload(nest(MaxObjectDepth)))
	require.NoError(t, ValidateDocumentPayload(map[string]any{
		// 数组透明：对象经数组包裹不增加深度。
		"arr": []any{map[string]any{"n": map[string]any{"v": 1}}},
	}))

	err := ValidateDocumentPayload(nest(MaxObjectDepth + 1))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "DOCUMENT.TOO_LARGE")

	// 数组内对象同样受深度约束。
	err = ValidateDocumentPayload(map[string]any{
		"arr": []any{nest(MaxObjectDepth + 1)["obj"]},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "DOCUMENT.TOO_LARGE")

	// 字符串长度不受深度语义影响（防 8 层嵌套误伤标量）。
	require.NoError(t, ValidateDocumentPayload(map[string]any{"s": strings.Repeat("a", 128)}))
}
