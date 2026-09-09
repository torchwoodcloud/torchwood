package users

import (
	"fmt"

	"github.com/torchwoodcloud/torchwood/pkg/query"
)

var (
	ErrInvalidListQuery = fmt.Errorf("invalid user list query")
	ErrInvalidUpdate    = fmt.Errorf("invalid user update")
)

// 系统表 List 只编译这些列；未知属性 → ErrInvalidListQuery（适配器映射 InvalidArgument）。
var listAttributes = map[string]struct{}{
	"email":      {},
	"name":       {},
	"status":     {},
	"phone":      {},
	"created_at": {},
	"updated_at": {},
}

var listOperators = map[string]struct{}{
	query.OpEqual:       {},
	query.OpGreaterThan: {},
	query.OpLessThan:    {},
}

// ParseUserList 用 pkg/query.ParseMany 解析 queries，并拒绝白名单外的属性/算子。
func ParseUserList(queries []string) (*query.Query, error) {
	parsed, err := query.ParseMany(queries)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidListQuery, err)
	}
	var first error
	parsed.WalkLeaves(func(f query.Filter) {
		if first != nil {
			return
		}
		if _, ok := listOperators[f.Op]; !ok {
			first = fmt.Errorf("%w: unsupported operator %q", ErrInvalidListQuery, f.Op)
			return
		}
		if _, ok := listAttributes[f.Attribute]; !ok {
			first = fmt.Errorf("%w: unknown attribute %q", ErrInvalidListQuery, f.Attribute)
		}
	})
	if first != nil {
		return nil, first
	}
	for _, o := range parsed.Orders {
		if _, ok := listAttributes[o.Attribute]; !ok {
			return nil, fmt.Errorf("%w: unknown attribute %q", ErrInvalidListQuery, o.Attribute)
		}
	}
	if len(parsed.Selects) > 0 || parsed.CursorAfter != "" || parsed.CursorBefore != "" {
		return nil, fmt.Errorf("%w: unsupported list clause", ErrInvalidListQuery)
	}
	return parsed, nil
}
