package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/documents"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// attrsDocDB 包装 fakeDocDB，使 GetCollection 返回带 n 个属性的集合，
// 供 CreateAttribute 列数软预算的单元测试使用。
type attrsDocDB struct {
	*fakeDocDB
	n int
}

func (a attrsDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	attrs := make([]databases.Attribute, 0, a.n)
	for i := 0; i < a.n; i++ {
		attrs = append(attrs, databases.Attribute{ID: fmt.Sprintf("c%d", i), Key: fmt.Sprintf("c%d", i), Type: "string"})
	}
	return &databases.Collection{ID: "coll1", Attributes: attrs}, nil
}

func nAttrs(n int) []databases.Attribute {
	attrs := make([]databases.Attribute, 0, n)
	for i := 0; i < n; i++ {
		attrs = append(attrs, databases.Attribute{ID: fmt.Sprintf("c%d", i), Key: fmt.Sprintf("c%d", i), Type: "string"})
	}
	return attrs
}

// TestCreateCollection_ColumnSoftLimit（redesign §11-J H2）：每集合列数软限
// 200（PG 1600 硬限留余量），超限 InvalidArgument / CATALOG.COLUMN_LIMIT_
// EXCEEDED，边界 200 列放行。
func TestCreateCollection_ColumnSoftLimit(t *testing.T) {
	d := &Databases{projectRepo: fakeProjectRepo{}, docDB: attrsDocDB{fakeDocDB: newFakeDocDB()}}
	ctx := platformAdminCtx(context.Background())

	err := d.CreateCollection(ctx, "p1", "db1", "coll1", "Coll", nAttrs(documents.MaxCollectionColumns+1), nil, nil, true)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "CATALOG.COLUMN_LIMIT_EXCEEDED")

	require.NoError(t, d.CreateCollection(ctx, "p1", "db1", "coll1", "Coll", nAttrs(documents.MaxCollectionColumns), nil, nil, true))
}

// TestCreateAttribute_ColumnSoftLimit（redesign §11-J H2）：存量 attrs + 本次
// 新增 ≤200，超限 InvalidArgument / CATALOG.COLUMN_LIMIT_EXCEEDED，边界
// 199+1=200 放行。
func TestCreateAttribute_ColumnSoftLimit(t *testing.T) {
	ctx := platformAdminCtx(context.Background())

	t.Run("over limit rejected", func(t *testing.T) {
		d := &Databases{projectRepo: fakeProjectRepo{}, docDB: attrsDocDB{fakeDocDB: newFakeDocDB(), n: documents.MaxCollectionColumns}}
		err := d.CreateAttribute(ctx, "p1", "db1", "coll1", databases.Attribute{ID: "next", Key: "next", Type: "string"})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "CATALOG.COLUMN_LIMIT_EXCEEDED")
	})

	t.Run("boundary passes", func(t *testing.T) {
		d := &Databases{projectRepo: fakeProjectRepo{}, docDB: attrsDocDB{fakeDocDB: newFakeDocDB(), n: documents.MaxCollectionColumns - 1}}
		require.NoError(t, d.CreateAttribute(ctx, "p1", "db1", "coll1", databases.Attribute{ID: "next", Key: "next", Type: "string"}))
	})
}

// TestExecuteTransactions_ACLTooLarge（redesign §11-J H2）：事务 op 的显式
// permissions 超 64 ACE 在进事务前拒绝，域码 DOCUMENT.ACL_TOO_LARGE 且
// BadRequest violations 定位到 ops[N].permissions；种子 op（≤3 条）不受影响。
func TestExecuteTransactions_ACLTooLarge(t *testing.T) {
	d := &Databases{projectRepo: fakeProjectRepo{}, docDB: attrsDocDB{fakeDocDB: newFakeDocDB()}}
	d.docs = documents.New(d.docDB, nil)
	ctx := platformAdminCtx(context.Background())

	perms := make([]databases.Permission, 0, documents.MaxDocumentACL+1)
	for i := 0; i < documents.MaxDocumentACL+1; i++ {
		perms = append(perms, databases.Permission{Type: "read", Role: fmt.Sprintf("group:g%d", i)})
	}
	_, _, err := d.ExecuteTransactions(ctx, "p1", "db1", []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "coll1", DocumentID: "d1",
			Data: map[string]any{"a": 1}, Permissions: perms},
	}, databases.TransactionModeAtomic, databases.Principal{PlatformAdmin: true}, "")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "DOCUMENT.ACL_TOO_LARGE")
	require.Contains(t, err.Error(), "ops[0].permissions")
}

// TestExecuteTransactions_ArrayUpdatesTooLarge（redesign §11-J H2 + 转出 POC
// B1）：事务 op 的 array_updates values 超 1000 元素在进事务前拒绝，
// DOCUMENT.TOO_LARGE 且 BadRequest violations 定位到 ops[N].array_updates
// （ValidateArrayUpdates 与单文档 Update 通道同源）。
func TestExecuteTransactions_ArrayUpdatesTooLarge(t *testing.T) {
	d := &Databases{projectRepo: fakeProjectRepo{}, docDB: attrsDocDB{fakeDocDB: newFakeDocDB()}}
	d.docs = documents.New(d.docDB, nil)
	ctx := platformAdminCtx(context.Background())

	values := make([]string, documents.MaxArrayElements+1)
	_, _, err := d.ExecuteTransactions(ctx, "p1", "db1", []databases.TransactionOp{
		{Type: databases.TransactionOpUpdate, CollectionID: "coll1", DocumentID: "d1",
			ArrayUpdates: map[string]databases.ArrayUpdate{
				"tags": {Op: databases.ArrayUpdateOpAppend, Values: values},
			}},
	}, databases.TransactionModeAtomic, databases.Principal{PlatformAdmin: true}, "")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "DOCUMENT.TOO_LARGE")
	require.Contains(t, err.Error(), "ops[0].array_updates")
}
