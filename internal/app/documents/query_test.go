package documents

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// C7 单 AST：ResolveQuery 只消费 AST（DSL 字符串栈已退役）。
func TestResolveQuery_ProtoAST(t *testing.T) {
	got, err := ResolveQuery(databases.Query{
		PageSize: 25,
		AST: &query.Query{
			Filter: query.Eq("a", "b"),
		},
	})
	require.NoError(t, err)
	require.Equal(t, query.OpEqual, got.Filter.Op)
	require.Equal(t, "a", got.Filter.Attribute)
	require.Equal(t, []string{"b"}, got.Filter.Values)
	require.Equal(t, int32(25), got.PageSize)
}

func TestResolveQuery_NilASTIsPlainList(t *testing.T) {
	got, err := ResolveQuery(databases.Query{PageSize: 10})
	require.NoError(t, err)
	require.Nil(t, got.Filter)
	require.Equal(t, int32(10), got.PageSize)
}

func TestResolveQuery_ConflictPageSize(t *testing.T) {
	_, err := ResolveQuery(databases.Query{
		PageSize: 10,
		AST: &query.Query{
			Filter:   query.Eq("a", "1"),
			PageSize: 20,
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestResolveQuery_SamePageSizeNotConflict(t *testing.T) {
	got, err := ResolveQuery(databases.Query{
		PageSize: 10,
		AST: &query.Query{
			Filter:   query.Eq("a", "1"),
			PageSize: 10,
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(10), got.PageSize)
}

func TestResolveQuery_ConflictPageToken(t *testing.T) {
	_, err := ResolveQuery(databases.Query{
		PageToken: "top",
		AST: &query.Query{
			Filter:    query.Eq("a", "1"),
			PageToken: "ast",
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestBindListQuery_EmptyGtEq(t *testing.T) {
	_, err := BindListQuery(0, "", &sharedv1.Query{
		Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Gt{Gt: &sharedv1.Comparison{Attribute: "$id"}}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = BindListQuery(0, "", &sharedv1.Query{
		Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: &sharedv1.Comparison{Attribute: "title"}}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestListCountDocuments_EmptyComparisonValues(t *testing.T) {
	core := New(newMemDocDB(), nil)
	ctx := context.Background()
	emptyGt := databases.Query{AST: &query.Query{Filter: &query.Filter{Op: query.OpGreaterThan, Attribute: "n"}}}
	_, err := core.ListDocuments(ctx, "p", "app", "notes", emptyGt, databases.Principal{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = core.CountDocuments(ctx, "p", "app", "notes", emptyGt, databases.Principal{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	emptyEq := databases.Query{AST: &query.Query{Filter: &query.Filter{Op: query.OpEqual, Attribute: "title"}}}
	_, err = core.ListDocuments(ctx, "p", "app", "notes", emptyEq, databases.Principal{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = core.CountDocuments(ctx, "p", "app", "notes", emptyEq, databases.Principal{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
