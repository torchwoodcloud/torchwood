// Package queryproto 把 shared.v1.Query 编进 AST。独立子包以免 pkg/query
// 把 genproto 带进 worker 等不碰 RPC 的进程。
package queryproto

import (
	"fmt"

	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// FromProto decodes a proto Query into the AST（C7 单 AST：proto Query 是
// 服务端唯一消费的查询形态）。nil input yields nil.
func FromProto(src *sharedv1.Query) (*query.Query, error) {
	if src == nil {
		return nil, nil
	}
	leaves := 0
	filter, err := filterFromProto(src.GetFilter(), 0, &leaves)
	if err != nil {
		return nil, err
	}
	out := &query.Query{
		Filter:    filter,
		PageSize:  src.GetPageSize(),
		PageToken: src.GetPageToken(),
		Selects:   append([]string{}, src.GetSelect()...),
	}
	if vs := src.GetVectorSearch(); vs != nil {
		v, err := vectorSearchFromProto(vs, &leaves)
		if err != nil {
			return nil, err
		}
		out.VectorSearch = v
	}
	if filter != nil && filter.Op != query.OpAnd && filter.Op != query.OpOr {
		out.Filters = []query.Filter{*filter}
	} else if filter != nil && filter.Op == query.OpAnd {
		for _, c := range filter.Children {
			if c != nil && c.Op != query.OpAnd && c.Op != query.OpOr {
				out.Filters = append(out.Filters, *c)
			}
		}
	}
	for _, o := range src.GetOrders() {
		if o == nil || o.GetAttribute() == "" {
			return nil, fmt.Errorf("order attribute is required")
		}
		out.Orders = append(out.Orders, query.Order{Attribute: o.GetAttribute(), Desc: o.GetDesc()})
	}
	// KNN 组合约束（会话 #10 预决策 3；B2 放开 page_token）：与 orders 互斥
	// ——排序由距离承载。page_token 合法（多页 KNN，B2）：续页 token 是
	// kvc: 距离游标，形态与归属校验在 infra 管道（documentdb）执行——codec
	// 无 schema 上下文，只做透传。page_size = k。
	if out.VectorSearch != nil && len(out.Orders) > 0 {
		return nil, fmt.Errorf("vector_search cannot be combined with orders; distance carries the ordering")
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// vectorSearchFromProto 解码 KNN 算子：metric 枚举映射（UNSPECIFIED →
// COSINE 缺省）；维度非空由服务端按 catalog dims 校验（codec 无 schema）。
func vectorSearchFromProto(src *sharedv1.VectorSearch, leaves *int) (*query.VectorSearch, error) {
	if src.GetAttribute() == "" {
		return nil, fmt.Errorf("vector_search attribute is required")
	}
	if len(src.GetValues()) == 0 {
		return nil, fmt.Errorf("vector_search requires at least 1 value")
	}
	metric := query.MetricCosine
	switch src.GetMetric() {
	case sharedv1.DistanceMetric_DISTANCE_METRIC_UNSPECIFIED, sharedv1.DistanceMetric_DISTANCE_METRIC_COSINE:
	case sharedv1.DistanceMetric_DISTANCE_METRIC_L2:
		metric = query.MetricL2
	case sharedv1.DistanceMetric_DISTANCE_METRIC_INNER_PRODUCT:
		metric = query.MetricInnerProduct
	default:
		return nil, fmt.Errorf("unsupported distance metric: %v", src.GetMetric())
	}
	if leaves != nil {
		*leaves++
		if *leaves > query.MaxQueries {
			return nil, fmt.Errorf("filter node count exceeds maximum of %d", query.MaxQueries)
		}
	}
	v := &query.VectorSearch{
		Attribute: src.GetAttribute(),
		Values:    append([]float64{}, src.GetValues()...),
		Metric:    metric,
	}
	if src.MaxDistance != nil {
		md := src.GetMaxDistance()
		v.MaxDistance = &md
	}
	// ef_search（B7）presence 透传；取值域 [1,500] 由服务端（infra 管道，
	// schema 上下文所在）显式拒绝——codec 只保留"是否设置"的语义。
	if src.EfSearch != nil {
		ef := src.GetEfSearch()
		v.EfSearch = &ef
	}
	return v, nil
}

func filterFromProto(src *sharedv1.Filter, depth int, leaves *int) (*query.Filter, error) {
	if src == nil {
		return nil, nil
	}
	if depth > query.MaxDepth {
		return nil, fmt.Errorf("filter nesting exceeds maximum of %d", query.MaxDepth)
	}
	switch e := src.GetExpr().(type) {
	case *sharedv1.Filter_Eq:
		return comparisonFilter(query.OpEqual, e.Eq, leaves)
	case *sharedv1.Filter_Ne:
		return comparisonFilter(query.OpNotEqual, e.Ne, leaves)
	case *sharedv1.Filter_Lt:
		return comparisonFilter(query.OpLessThan, e.Lt, leaves)
	case *sharedv1.Filter_Lte:
		return comparisonFilter(query.OpLessThanEqual, e.Lte, leaves)
	case *sharedv1.Filter_Gt:
		return comparisonFilter(query.OpGreaterThan, e.Gt, leaves)
	case *sharedv1.Filter_Gte:
		return comparisonFilter(query.OpGreaterThanEqual, e.Gte, leaves)
	case *sharedv1.Filter_In:
		return comparisonFilter(query.OpIn, e.In, leaves)
	case *sharedv1.Filter_Contains:
		return comparisonFilter(query.OpContains, e.Contains, leaves)
	case *sharedv1.Filter_StartsWith:
		return comparisonFilter(query.OpStartsWith, e.StartsWith, leaves)
	case *sharedv1.Filter_EndsWith:
		return comparisonFilter(query.OpEndsWith, e.EndsWith, leaves)
	case *sharedv1.Filter_Search:
		return comparisonFilter(query.OpSearch, e.Search, leaves)
	case *sharedv1.Filter_Between:
		return comparisonFilter(query.OpBetween, e.Between, leaves)
	case *sharedv1.Filter_IsNull:
		return comparisonFilter(query.OpIsNull, e.IsNull, leaves)
	case *sharedv1.Filter_IsNotNull:
		return comparisonFilter(query.OpIsNotNull, e.IsNotNull, leaves)
	case *sharedv1.Filter_NotBetween:
		return comparisonFilter(query.OpNotBetween, e.NotBetween, leaves)
	case *sharedv1.Filter_NotContains:
		return comparisonFilter(query.OpNotContains, e.NotContains, leaves)
	case *sharedv1.Filter_NotStartsWith:
		return comparisonFilter(query.OpNotStartsWith, e.NotStartsWith, leaves)
	case *sharedv1.Filter_NotEndsWith:
		return comparisonFilter(query.OpNotEndsWith, e.NotEndsWith, leaves)
	case *sharedv1.Filter_NotSearch:
		return comparisonFilter(query.OpNotSearch, e.NotSearch, leaves)
	case *sharedv1.Filter_ContainsAny:
		return comparisonFilter(query.OpContainsAny, e.ContainsAny, leaves)
	case *sharedv1.Filter_ContainsAll:
		return comparisonFilter(query.OpContainsAll, e.ContainsAll, leaves)
	case *sharedv1.Filter_And:
		return boolFilter(query.OpAnd, e.And, depth, leaves)
	case *sharedv1.Filter_Or:
		return boolFilter(query.OpOr, e.Or, depth, leaves)
	case nil:
		return nil, fmt.Errorf("filter expr is required")
	default:
		return nil, fmt.Errorf("unsupported filter expr")
	}
}

// comparisonFilter 按 §4.1 的算子值数量约束分流：between/notBetween 恰 2；
// isNull/isNotNull 0；其余 ≥1。
func comparisonFilter(op string, c *sharedv1.Comparison, leaves *int) (*query.Filter, error) {
	if c == nil {
		return nil, fmt.Errorf("%s comparison is required", op)
	}
	if c.GetAttribute() == "" {
		return nil, fmt.Errorf("%s attribute is required", op)
	}
	values := append([]string{}, c.GetValues()...)
	switch op {
	case query.OpBetween, query.OpNotBetween:
		if len(values) != 2 {
			return nil, fmt.Errorf("%s requires exactly 2 values", op)
		}
	case query.OpIsNull, query.OpIsNotNull:
		// 无值算子：proto 端多余 values 直接拒绝（而非静默忽略）。
		if len(values) != 0 {
			return nil, fmt.Errorf("%s takes no values", op)
		}
	default:
		if len(values) < 1 {
			return nil, fmt.Errorf("%s requires at least 1 value", op)
		}
	}
	if leaves != nil {
		*leaves++
		if *leaves > query.MaxQueries {
			return nil, fmt.Errorf("filter node count exceeds maximum of %d", query.MaxQueries)
		}
	}
	return &query.Filter{Op: op, Attribute: c.GetAttribute(), Values: values}, nil
}

func boolFilter(op string, list *sharedv1.FilterList, depth int, leaves *int) (*query.Filter, error) {
	if list == nil || len(list.GetFilters()) == 0 {
		return nil, fmt.Errorf("%s requires at least one filter", op)
	}
	if len(list.GetFilters()) > query.MaxQueries {
		return nil, fmt.Errorf("filter node count exceeds maximum of %d", query.MaxQueries)
	}
	children := make([]*query.Filter, 0, len(list.GetFilters()))
	for _, child := range list.GetFilters() {
		f, err := filterFromProto(child, depth+1, leaves)
		if err != nil {
			return nil, err
		}
		if f != nil {
			children = append(children, f)
		}
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("%s requires at least one filter", op)
	}
	if len(children) == 1 {
		return children[0], nil
	}
	return &query.Filter{Op: op, Children: children}, nil
}
