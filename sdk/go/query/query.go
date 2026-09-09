// Package query 提供 torchwood 文档查询的 typed 构造器与 DSL 语法糖
// （C7 单 AST：服务端只收 shared.v1.Query；DSL 串在客户端解析为 AST 后发送）。
//
// 构造器产出 *sharedv1.Query，供 sdk/go 的 client/server 两面与直接调用
// gRPC/HTTP 网关的用户使用。DSL 语法糖 FromDSL 的文法与仓库根模块的
// pkg/query 同源（Appwrite 风格）；本包自带解析器以保持 SDK 模块的依赖
// 面最小（仅 genproto + stdlib），文法演进需两侧同步（sdk/go/query 与
// pkg/query 的 golden 用例锁对齐）。
package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// ---------------------------------------------------------------------------
// Filter 叶子构造器（Comparison 形态）
// ---------------------------------------------------------------------------

// comparison 包一个 Comparison。
func comparison(attr string, values []string) *sharedv1.Comparison {
	return &sharedv1.Comparison{Attribute: attr, Values: values}
}

// Eq 等值；多值时语义为 IN（与服务端编译一致）。
func Eq(attr string, values ...string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: comparison(attr, values)}}
}

// Ne 不等；多值时语义为 NOT IN。
func Ne(attr string, values ...string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Ne{Ne: comparison(attr, values)}}
}

func Lt(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Lt{Lt: comparison(attr, []string{value})}}
}

func Lte(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Lte{Lte: comparison(attr, []string{value})}}
}

func Gt(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Gt{Gt: comparison(attr, []string{value})}}
}

func Gte(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Gte{Gte: comparison(attr, []string{value})}}
}

// In 集合成员（值列表）。
func In(attr string, values ...string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_In{In: comparison(attr, values)}}
}

// ContainsAny / ContainsAll 是数组算子（§10.5 P0）：仅 array=true 属性可用，
// 服务端编译为 PG &&（交集非空）/ @>（子集）。
func ContainsAny(attr string, values ...string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_ContainsAny{ContainsAny: comparison(attr, values)}}
}

func ContainsAll(attr string, values ...string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_ContainsAll{ContainsAll: comparison(attr, values)}}
}

func Contains(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Contains{Contains: comparison(attr, []string{value})}}
}

func NotContains(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_NotContains{NotContains: comparison(attr, []string{value})}}
}

func StartsWith(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_StartsWith{StartsWith: comparison(attr, []string{value})}}
}

func NotStartsWith(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_NotStartsWith{NotStartsWith: comparison(attr, []string{value})}}
}

func EndsWith(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_EndsWith{EndsWith: comparison(attr, []string{value})}}
}

func NotEndsWith(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_NotEndsWith{NotEndsWith: comparison(attr, []string{value})}}
}

// Search 全文检索（目标属性须有 fulltext 索引）。
func Search(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Search{Search: comparison(attr, []string{value})}}
}

func NotSearch(attr, value string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_NotSearch{NotSearch: comparison(attr, []string{value})}}
}

// Between 闭区间 [min, max]。
func Between(attr, min, max string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Between{Between: comparison(attr, []string{min, max})}}
}

func NotBetween(attr, min, max string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_NotBetween{NotBetween: comparison(attr, []string{min, max})}}
}

// IsNull / IsNotNull 空值判定（无值算子）。
func IsNull(attr string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_IsNull{IsNull: comparison(attr, nil)}}
}

func IsNotNull(attr string) *sharedv1.Filter {
	return &sharedv1.Filter{Expr: &sharedv1.Filter_IsNotNull{IsNotNull: comparison(attr, nil)}}
}

// And / Or 组合布尔节点（嵌套深度 ≤8）；nil 子节点被跳过，单个子节点坍缩。
func And(children ...*sharedv1.Filter) *sharedv1.Filter {
	kept := keepNonNil(children)
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return &sharedv1.Filter{Expr: &sharedv1.Filter_And{And: &sharedv1.FilterList{Filters: kept}}}
}

func Or(children ...*sharedv1.Filter) *sharedv1.Filter {
	kept := keepNonNil(children)
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return &sharedv1.Filter{Expr: &sharedv1.Filter_Or{Or: &sharedv1.FilterList{Filters: kept}}}
}

func keepNonNil(children []*sharedv1.Filter) []*sharedv1.Filter {
	kept := make([]*sharedv1.Filter, 0, len(children))
	for _, c := range children {
		if c != nil {
			kept = append(kept, c)
		}
	}
	return kept
}

// ---------------------------------------------------------------------------
// VectorSearch（KNN 算子，会话 #10；B2 多页）：非 filter 节点——距离承载
// 排序，limit 即 k（top-k 可见近邻）。DSL 字符串不支持 vector_search（向量
// 不该手写，typed builder only）；与 orders 互斥（服务端拒绝）。多页翻页：
// 同 Query 以 PageToken 携带服务端发放的 kvc: 距离游标（Builder.PageToken
// / DocumentsPager 均可透传）。
// ---------------------------------------------------------------------------

// VectorSearchBuilder 链式构造 shared.v1.VectorSearch。
type VectorSearchBuilder struct {
	v sharedv1.VectorSearch
}

// VectorSearch 创建 KNN 查询构造器（metric 缺省 COSINE）。values 须与目标
// vector 属性的声明维度等长（服务端按 catalog 校验）；目标属性须存在与
// metric 匹配的 hnsw 索引。
func VectorSearch(attribute string, values ...float64) *VectorSearchBuilder {
	b := &VectorSearchBuilder{}
	b.v.Attribute = attribute
	b.v.Values = append([]float64{}, values...)
	b.v.Metric = sharedv1.DistanceMetric_DISTANCE_METRIC_COSINE
	return b
}

// MetricCosine 余弦距离（PG <=>，hnsw COSINE 索引）。
func (b *VectorSearchBuilder) MetricCosine() *VectorSearchBuilder {
	b.v.Metric = sharedv1.DistanceMetric_DISTANCE_METRIC_COSINE
	return b
}

// MetricL2 欧氏距离（PG <->，hnsw L2 索引）。
func (b *VectorSearchBuilder) MetricL2() *VectorSearchBuilder {
	b.v.Metric = sharedv1.DistanceMetric_DISTANCE_METRIC_L2
	return b
}

// MetricInnerProduct 负内积（PG <#>，hnsw INNER_PRODUCT 索引；值域
// (-inf, 0]，越小越近）。
func (b *VectorSearchBuilder) MetricInnerProduct() *VectorSearchBuilder {
	b.v.Metric = sharedv1.DistanceMetric_DISTANCE_METRIC_INNER_PRODUCT
	return b
}

// MaxDistance 距离阈值：仅保留 top-k 中距离 <= max 的行（服务端后置过滤）。
func (b *VectorSearchBuilder) MaxDistance(max float64) *VectorSearchBuilder {
	md := max
	b.v.MaxDistance = &md
	return b
}

// EfSearch HNSW 搜索广度（B7）：本次查询的 hnsw.ef_search，合法域 [1,500]
// （≤0 / >500 服务端 InvalidArgument）。缺省不设置——服务端维持 pgvector
// 缺省 40，行为与未提供时一致。近重复簇边界召回不足时调大（代价：访存
// 与延迟随 ef 线性增长）。
func (b *VectorSearchBuilder) EfSearch(n int32) *VectorSearchBuilder {
	ef := n
	b.v.EfSearch = &ef
	return b
}

// Build 产出 *shared.v1.VectorSearch。
func (b *VectorSearchBuilder) Build() *sharedv1.VectorSearch {
	out := &sharedv1.VectorSearch{
		Attribute: b.v.GetAttribute(),
		Metric:    b.v.GetMetric(),
	}
	out.Values = append(out.Values, b.v.GetValues()...)
	if b.v.MaxDistance != nil {
		md := b.v.GetMaxDistance()
		out.MaxDistance = &md
	}
	if b.v.EfSearch != nil {
		ef := b.v.GetEfSearch()
		out.EfSearch = &ef
	}
	return out
}

// ---------------------------------------------------------------------------
// Query 构造器（链式）
// ---------------------------------------------------------------------------

// Builder 链式构造 shared.v1.Query。
type Builder struct {
	q sharedv1.Query
}

// New 创建空 Builder。
func New() *Builder { return &Builder{} }

// Filter 设置过滤树（多次调用后者覆盖）。
func (b *Builder) Filter(f *sharedv1.Filter) *Builder {
	b.q.Filter = f
	return b
}

// VectorSearch 设置 KNN 算子（会话 #10；B2 多页：可与 PageToken 组合翻页，
// 服务端发放 kvc: 距离游标；与 OrderAsc/OrderDesc 互斥——服务端显式拒绝
// 非法组合）。PageSize 即 k。
func (b *Builder) VectorSearch(v *sharedv1.VectorSearch) *Builder {
	b.q.VectorSearch = v
	return b
}

// OrderAsc 追加升序排序键（服务端强制追加 $id tiebreaker）。
func (b *Builder) OrderAsc(attr string) *Builder {
	b.q.Orders = append(b.q.Orders, &sharedv1.Order{Attribute: attr})
	return b
}

// OrderDesc 追加降序排序键。
func (b *Builder) OrderDesc(attr string) *Builder {
	b.q.Orders = append(b.q.Orders, &sharedv1.Order{Attribute: attr, Desc: true})
	return b
}

// Select 投影：只返回这些字段。
func (b *Builder) Select(fields ...string) *Builder {
	b.q.Select = append(b.q.Select, fields...)
	return b
}

// PageSize 页大小（1..200；0 = 服务端默认）。
func (b *Builder) PageSize(n int32) *Builder {
	b.q.PageSize = n
	return b
}

// PageToken 续页 token（服务端返回的 ka:/kb: keyset token）。
func (b *Builder) PageToken(tok string) *Builder {
	b.q.PageToken = tok
	return b
}

// Build 产出 *sharedv1.Query（proto 消息含锁不可值拷贝，逐字段组装）。
func (b *Builder) Build() *sharedv1.Query {
	out := &sharedv1.Query{
		PageSize:  b.q.GetPageSize(),
		PageToken: b.q.GetPageToken(),
	}
	out.Filter = b.q.GetFilter()
	out.VectorSearch = b.q.GetVectorSearch()
	out.Orders = append(out.Orders, b.q.GetOrders()...)
	out.Select = append(out.Select, b.q.GetSelect()...)
	return out
}

// ---------------------------------------------------------------------------
// DSL 语法糖：Appwrite 风格串 → *sharedv1.Query（客户端解析，服务端零消费）
// ---------------------------------------------------------------------------

var dslRe = regexp.MustCompile(`^(\w+)\((.*)\)$`)

// 输入上限：与 pkg/query 的 MaxQueries/MaxQueryLen 同值（模块独立无法共享
// 常量，文法契约由共享 golden 语料锁定——pkg/query/testdata/dsl_ast_golden.json）。
const (
	dslMaxQueries  = 100
	dslMaxQueryLen = 4096
)

// FromDSL 把 Appwrite 风格 DSL 串解析为 typed Query（隐式 AND 合并）。
// 支持算子：equal/notEqual/lessThan/lessThanEqual/greaterThan/
// greaterThanEqual/in/contains/notContains/startsWith/notStartsWith/
// endsWith/notEndsWith/search/notSearch/between/notBetween/isNull/
// isNotNull/orderAsc/orderDesc/limit/select/cursorAfter/cursorBefore。
// offset() 不支持（文档面 keyset-only）；cursorAfter/Before 映射为
// ka:/kb: keyset page token。
func FromDSL(parts ...string) (*sharedv1.Query, error) {
	if len(parts) > dslMaxQueries {
		return nil, fmt.Errorf("queries count exceeds maximum of %d", dslMaxQueries)
	}
	q := &sharedv1.Query{}
	var leaves []*sharedv1.Filter
	for _, raw := range parts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if len(raw) > dslMaxQueryLen {
			return nil, fmt.Errorf("query string exceeds maximum length of %d", dslMaxQueryLen)
		}
		m := dslRe.FindStringSubmatch(raw)
		if m == nil {
			return nil, fmt.Errorf("invalid query format: %s", raw)
		}
		op, argStr := m[1], m[2]
		args, err := splitArgs(argStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse query %q: %w", raw, err)
		}
		switch op {
		case "equal", "notEqual", "lessThan", "lessThanEqual", "greaterThan", "greaterThanEqual",
			"contains", "notContains", "startsWith", "notStartsWith", "endsWith", "notEndsWith",
			"search", "notSearch":
			if len(args) < 2 {
				return nil, fmt.Errorf("%s requires at least 2 args", op)
			}
			attr, err := unquote(args[0])
			if err != nil {
				return nil, err
			}
			var values []string
			if strings.HasPrefix(args[1], "[") {
				values, err = parseArray(args[1])
				if err != nil {
					return nil, err
				}
			} else {
				v, err := unquoteOrLiteral(args[1])
				if err != nil {
					return nil, err
				}
				values = []string{v}
			}
			leaf, err := dslLeaf(op, attr, values)
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, leaf)
		case "in":
			if len(args) != 2 {
				return nil, fmt.Errorf("in requires 2 args")
			}
			attr, err := unquote(args[0])
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(args[1], "[") {
				return nil, fmt.Errorf("in requires an array of values, e.g. in(%q, [\"a\",\"b\"])", attr)
			}
			values, err := parseArray(args[1])
			if err != nil {
				return nil, err
			}
			if len(values) == 0 {
				return nil, fmt.Errorf("in requires at least 1 value")
			}
			leaves = append(leaves, In(attr, values...))
		case "containsAny", "containsAll":
			// 数组算子（§10.5 P0）：与 in 同构，值必须是数组；仅 array=true
			// 属性可用（服务端按 catalog attrs 白名单校验）。
			if len(args) != 2 {
				return nil, fmt.Errorf("%s requires 2 args", op)
			}
			attr, err := unquote(args[0])
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(args[1], "[") {
				return nil, fmt.Errorf("%s requires an array of values, e.g. %s(%q, [\"a\",\"b\"])", op, op, attr)
			}
			values, err := parseArray(args[1])
			if err != nil {
				return nil, err
			}
			if len(values) == 0 {
				return nil, fmt.Errorf("%s requires at least 1 value", op)
			}
			if op == "containsAny" {
				leaves = append(leaves, ContainsAny(attr, values...))
			} else {
				leaves = append(leaves, ContainsAll(attr, values...))
			}
		case "between", "notBetween":
			if len(args) != 3 {
				return nil, fmt.Errorf("%s requires 3 args", op)
			}
			attr, err := unquote(args[0])
			if err != nil {
				return nil, err
			}
			min, err := unquoteOrLiteral(args[1])
			if err != nil {
				return nil, err
			}
			max, err := unquoteOrLiteral(args[2])
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, dslBetween(op, attr, min, max))
		case "isNull", "isNotNull":
			if len(args) != 1 {
				return nil, fmt.Errorf("%s requires 1 arg", op)
			}
			attr, err := unquote(args[0])
			if err != nil {
				return nil, err
			}
			if op == "isNull" {
				leaves = append(leaves, IsNull(attr))
			} else {
				leaves = append(leaves, IsNotNull(attr))
			}
		case "orderAsc", "orderDesc":
			if len(args) != 1 {
				return nil, fmt.Errorf("%s requires 1 arg", op)
			}
			attr, err := unquote(args[0])
			if err != nil {
				return nil, err
			}
			q.Orders = append(q.Orders, &sharedv1.Order{Attribute: attr, Desc: op == "orderDesc"})
		case "limit":
			if len(args) != 1 {
				return nil, fmt.Errorf("limit requires 1 arg")
			}
			// 错误文案与 pkg/query 逐字一致（golden 语料锁 substring）。
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return nil, fmt.Errorf("limit must be an integer")
			}
			if n < 0 {
				return nil, fmt.Errorf("limit must be non-negative")
			}
			if n > 0 {
				q.PageSize = int32(n)
			}
		case "select":
			if len(args) != 1 {
				return nil, fmt.Errorf("select requires 1 arg")
			}
			fields, err := parseArray(args[0])
			if err != nil {
				return nil, err
			}
			q.Select = append(q.Select, fields...)
		case "cursorAfter", "cursorBefore":
			if len(args) != 1 {
				return nil, fmt.Errorf("%s requires 1 arg", op)
			}
			id, err := unquote(args[0])
			if err != nil {
				return nil, err
			}
			if op == "cursorAfter" {
				q.PageToken = "ka:" + id
			} else {
				q.PageToken = "kb:" + id
			}
		case "offset":
			return nil, fmt.Errorf("offset() is not supported; documents use cursor (keyset) pagination")
		default:
			return nil, fmt.Errorf("unsupported query operator: %s", op)
		}
	}
	if len(leaves) == 1 {
		q.Filter = leaves[0]
	} else if len(leaves) > 1 {
		q.Filter = And(leaves...)
	}
	return q, nil
}

func dslLeaf(op, attr string, values []string) (*sharedv1.Filter, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("%s requires at least 1 value", op)
	}
	switch op {
	case "equal":
		return Eq(attr, values...), nil
	case "notEqual":
		return Ne(attr, values...), nil
	case "lessThan":
		return Lt(attr, values[0]), nil
	case "lessThanEqual":
		return Lte(attr, values[0]), nil
	case "greaterThan":
		return Gt(attr, values[0]), nil
	case "greaterThanEqual":
		return Gte(attr, values[0]), nil
	case "contains":
		return Contains(attr, values[0]), nil
	case "notContains":
		return NotContains(attr, values[0]), nil
	case "startsWith":
		return StartsWith(attr, values[0]), nil
	case "notStartsWith":
		return NotStartsWith(attr, values[0]), nil
	case "endsWith":
		return EndsWith(attr, values[0]), nil
	case "notEndsWith":
		return NotEndsWith(attr, values[0]), nil
	case "search":
		return Search(attr, values[0]), nil
	default:
		return NotSearch(attr, values[0]), nil
	}
}

func dslBetween(op, attr, min, max string) *sharedv1.Filter {
	if op == "between" {
		return Between(attr, min, max)
	}
	return NotBetween(attr, min, max)
}

// splitArgs 按顶层逗号分割（尊重引号与括号嵌套）。
func splitArgs(s string) ([]string, error) {
	var args []string
	var sb strings.Builder
	depth := 0
	inQuote := false
	var escape bool
	for _, r := range s {
		if escape {
			sb.WriteRune(r)
			escape = false
			continue
		}
		if r == '\\' {
			escape = true
			sb.WriteRune(r)
			continue
		}
		if r == '"' {
			inQuote = !inQuote
			sb.WriteRune(r)
			continue
		}
		if inQuote {
			sb.WriteRune(r)
			continue
		}
		switch r {
		case '[', '(':
			depth++
			sb.WriteRune(r)
		case ']', ')':
			depth--
			sb.WriteRune(r)
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(sb.String()))
				sb.Reset()
			} else {
				sb.WriteRune(r)
			}
		default:
			sb.WriteRune(r)
		}
	}
	if inQuote || depth != 0 {
		return nil, fmt.Errorf("unbalanced arguments: %s", s)
	}
	if sb.Len() > 0 {
		args = append(args, strings.TrimSpace(sb.String()))
	}
	return args, nil
}

func unquote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("expected quoted string, got %s", s)
	}
	return unescape(s[1 : len(s)-1]), nil
}

func unquoteOrLiteral(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return unescape(s[1 : len(s)-1]), nil
	}
	return s, nil
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseArray(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("expected array, got %s", s)
	}
	items, err := splitArgs(s[1 : len(s)-1])
	if err != nil {
		return nil, err
	}
	for i := range items {
		v, err := unquoteOrLiteral(items[i])
		if err != nil {
			return nil, err
		}
		items[i] = v
	}
	return items, nil
}
