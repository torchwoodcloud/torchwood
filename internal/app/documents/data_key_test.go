package documents

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestValidateDocumentPayload_KeyGuard（S6 缺陷 1）：数据键前置校验——`_` 前缀
// 系统列保留、标识符语法、≤63 字节，非法键 InvalidArgument / DOCUMENT.INVALID_
// ARGUMENT 且 violations 定位到 data.<k>；合法键（含 63 字节边界）放行。
func TestValidateDocumentPayload_KeyGuard(t *testing.T) {
	// 合法键放行（63 字节边界含内）。
	require.NoError(t, ValidateDocumentPayload(map[string]any{
		"name": "x", "email_verified": true, strings.Repeat("x", 63): 1,
	}))

	invalid := []struct {
		key    string
		reason string
	}{
		{"my key", "invalid attribute key"},
		{"a.b", "invalid attribute key"},
		{"_foo", "reserved for system columns"},
		{"_id", "reserved for system columns"},
		{strings.Repeat("x", 64), "identifier limit"},
	}
	for _, tc := range invalid {
		err := ValidateDocumentPayload(map[string]any{tc.key: 1})
		require.Error(t, err, "key %q", tc.key)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "key %q", tc.key)
		require.Contains(t, err.Error(), "DOCUMENT.INVALID_ARGUMENT", "key %q", tc.key)
		require.Contains(t, err.Error(), "data."+tc.key, "violations 必须定位到 data.<k>")
		require.Contains(t, err.Error(), tc.key, "错误信息必须携带键名")
		require.Contains(t, err.Error(), tc.reason, "key %q", tc.key)
	}
}

// TestValidateIncrement_KeyGuard（S6 缺陷 1）：increment 通道键名前置校验，
// violations 定位到 increment.<k>；UpdateDocument 在进 adapter 前早拒。
func TestValidateIncrement_KeyGuard(t *testing.T) {
	require.NoError(t, ValidateIncrement(map[string]int64{"views": 1}))

	err := ValidateIncrement(map[string]int64{"my key": 1})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "increment.my key")

	err = ValidateIncrement(map[string]int64{"_views": 1})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "reserved for system columns")

	// UpdateDocument 早拒（错误信息带字段名，省一次 DB 往返）：adapter 的
	// UpdateDocument 对缺失文档返回 NotFound，收到 InvalidArgument 即证明
	// 校验先于 adapter。
	ctx := context.Background()
	principal := databases.Principal{Roles: []string{"keys"}}
	core := New(newMemDocDB(), nil)
	v := int64(1)
	_, _, err = core.UpdateDocument(ctx, "p", "app", "notes", "d1", nil, nil,
		map[string]int64{"my key": 1}, nil, principal, &v, "", WriteOptions{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "increment.my key")
}

// TestCreateDocument_IllegalKeyRejectedBeforeAdapter（S6 缺陷 1）：create /
// upsert / bulk 三路径非法键在 app 层早拒，adapter 零调用。
func TestCreateDocument_IllegalKeyRejectedBeforeAdapter(t *testing.T) {
	ctx := context.Background()
	principal := databases.Principal{Roles: []string{"keys"}}
	opts := WriteOptions{AllowPrivilegedGrant: true}

	rec := newMemDocDB()
	core := New(rec, nil)
	_, _, err := core.CreateDocument(ctx, "p", "app", "notes", "d1", map[string]any{"my key": 1}, nil, principal, "", opts)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "data.my key")
	require.Zero(t, rec.creates)

	_, _, err = core.UpsertDocument(ctx, "p", "app", "notes", "d1", map[string]any{"a.b": "x"}, []string{"x"}, nil, principal, "", opts)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "data.a.b")
	require.Zero(t, rec.upserts)

	_, _, err = core.BulkUpdateDocuments(ctx, "p", "app", "notes", []string{"d1"}, map[string]any{"_foo": 1}, nil, principal, "", opts)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "reserved for system columns")
	require.Zero(t, rec.bulkUpdates)
}
