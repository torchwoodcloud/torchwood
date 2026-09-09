package documents

import (
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	queryproto "github.com/torchwoodcloud/torchwood/pkg/query/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BindListQuery 把 List/Count/Aggregate 传输字段编进 domain Query（C7 单 AST：
// proto codec 在此完成，handler 不手写解析；queries 字符串栈已退役）。
// pageSize/pageToken 是 GET 面的简单分页参数，与 Query 内同名字段冲突（不等）即拒。
func BindListQuery(pageSize int32, pageToken string, protoQ *sharedv1.Query) (databases.Query, error) {
	ast, err := queryproto.FromProto(protoQ)
	if err != nil {
		return databases.Query{}, status.Errorf(codes.InvalidArgument, "invalid query: %v", err)
	}
	return databases.Query{
		PageSize:  pageSize,
		PageToken: pageToken,
		AST:       ast,
	}, nil
}

// ResolveQuery 是 List/Count/Aggregate 的唯一 AST 入口：合并 GET 面分页字段
// 后校验。AST 缺省（无过滤的 plain list）合法。
func ResolveQuery(q databases.Query) (*query.Query, error) {
	out := cloneAST(q.AST)
	if err := mergePage(out, q.PageSize, q.PageToken); err != nil {
		return nil, err
	}
	if err := out.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid query: %v", err)
	}
	return out, nil
}

func mergePage(ast *query.Query, pageSize int32, pageToken string) error {
	if ast.PageSize == 0 {
		ast.PageSize = pageSize
	} else if pageSize != 0 && pageSize != ast.PageSize {
		return status.Error(codes.InvalidArgument, "query.page_size conflicts with page_size")
	}
	if ast.PageToken == "" {
		ast.PageToken = pageToken
	} else if pageToken != "" && pageToken != ast.PageToken {
		return status.Error(codes.InvalidArgument, "query.page_token conflicts with page_token")
	}
	return nil
}

func cloneAST(src *query.Query) *query.Query {
	if src == nil {
		return &query.Query{}
	}
	cp := *src
	return &cp
}
