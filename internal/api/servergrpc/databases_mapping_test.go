package servergrpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
)

// TestMapCollection_AttributeDefaultValue：GetCollection/ListCollections 响应必须
// 回填 default_value（catalog 落库后响应层不得再是断点）。
func TestMapCollection_AttributeDefaultValue(t *testing.T) {
	c := &databases.Collection{
		ID:         "posts",
		DatabaseID: "app",
		Name:       "Posts",
		Attributes: []databases.Attribute{
			{ID: "title", Key: "title", Type: "string", Size: 128, Default: "untitled"},
			{ID: "views", Key: "views", Type: "integer", Default: 42},
			{ID: "pinned", Key: "pinned", Type: "boolean"},
		},
	}
	out := mapCollection(c)
	require.Len(t, out.Attributes, 3)
	require.Equal(t, "untitled", out.Attributes[0].DefaultValue)
	require.Equal(t, "42", out.Attributes[1].DefaultValue)
	require.Empty(t, out.Attributes[2].DefaultValue)
}

// TestMapArrayUpdates_SetFamily（转出 POC B1）：四新算子 proto → domain 映射
// （含 insert index presence 透传）与 UNSPECIFIED 拒绝；TransactionOp.array_updates
// 经 transactionOpFromProto 全链透传。
func TestMapArrayUpdates_SetFamily(t *testing.T) {
	idx := int32(2)
	out, err := mapArrayUpdates(map[string]*sharedv1.ArrayUpdate{
		"tags": {Op: sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_INTERSECT, Values: []string{"a", "b"}},
		"nums": {Op: sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_DIFF, Values: []string{"1"}},
		"pos":  {Op: sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_INSERT, Values: []string{"x"}, Index: &idx},
		"flt":  {Op: sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_FILTER, Values: []string{"z"}},
		"uni":  {Op: sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_UNIQUE},
	})
	require.NoError(t, err)
	require.Equal(t, databases.ArrayUpdateOpIntersect, out["tags"].Op)
	require.Equal(t, databases.ArrayUpdateOpDiff, out["nums"].Op)
	require.Equal(t, databases.ArrayUpdateOpInsert, out["pos"].Op)
	require.NotNil(t, out["pos"].Index)
	require.Equal(t, int32(2), *out["pos"].Index)
	require.Equal(t, databases.ArrayUpdateOpFilter, out["flt"].Op)
	require.Equal(t, databases.ArrayUpdateOpUnique, out["uni"].Op)
	require.Nil(t, out["uni"].Index)

	_, err = mapArrayUpdates(map[string]*sharedv1.ArrayUpdate{
		"tags": {Op: sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_UNSPECIFIED},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	op, err := transactionOpFromProto(&serverv1.TransactionOp{
		Type:         serverv1.TransactionOpType_TRANSACTION_OP_TYPE_UPDATE,
		CollectionId: "items",
		DocumentId:   "d1",
		ArrayUpdates: map[string]*sharedv1.ArrayUpdate{
			"tags": {Op: sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_INSERT, Values: []string{"x"}, Index: &idx},
		},
	})
	require.NoError(t, err)
	require.Len(t, op.ArrayUpdates, 1)
	require.Equal(t, databases.ArrayUpdateOpInsert, op.ArrayUpdates["tags"].Op)
	require.NotNil(t, op.ArrayUpdates["tags"].Index)
	require.Equal(t, int32(2), *op.ArrayUpdates["tags"].Index)
}
