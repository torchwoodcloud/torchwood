package query

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Canonical AST operators. Appwrite codec names are the comparison ops;
// proto eq/ne/lt/... map onto these（codec 在 pkg/query/proto）。取反一律走
// not* 变体（索引友好）；无通用 NOT 算子（德摩根展开可表达，C7 预决策 1）。
const (
	OpEqual            = "equal"
	OpNotEqual         = "notEqual"
	OpLessThan         = "lessThan"
	OpLessThanEqual    = "lessThanEqual"
	OpGreaterThan      = "greaterThan"
	OpGreaterThanEqual = "greaterThanEqual"
	OpContains         = "contains"
	OpNotContains      = "notContains"
	OpStartsWith       = "startsWith"
	OpNotStartsWith    = "notStartsWith"
	OpEndsWith         = "endsWith"
	OpNotEndsWith      = "notEndsWith"
	OpSearch           = "search"
	OpNotSearch        = "notSearch"
	OpIsNull           = "isNull"
	OpIsNotNull        = "isNotNull"
	OpBetween          = "between"
	OpNotBetween       = "notBetween"
	OpIn               = "in"
	// 数组算子（§10.5 P0）：仅 array=true 属性可用（服务端白名单校验）。
	// containsAny = 交集非空；containsAll = 子集。值必须是数组。
	OpContainsAny = "containsAny"
	OpContainsAll = "containsAll"
	OpAnd         = "and"
	OpOr          = "or"
)

// Codec input limits (A2). documentdb still clamps SQL-side IN arity.
// MaxDepth：and/or 嵌套深度上限（§4.1 深度 ≤8；单 AST 会话从 16 收紧）。
const (
	MaxQueries  = 100
	MaxQueryLen = 4096
	MaxDepth    = 8
)

// Filter is a boolean expression node: a comparison leaf or and/or.
type Filter struct {
	Op        string
	Attribute string
	Values    []string
	Children  []*Filter // and / or
}

// Order represents an ordering clause.
type Order struct {
	Attribute string
	Desc      bool
}

// VectorSearch 是 KNN 算子（会话 #10 §10.5 P0）：非 filter 树节点——距离
// 排序承载顺序，limit 即 k。Metric 是归一后的大写形态（COSINE|L2|
// INNER_PRODUCT）；MaxDistance nil = 无阈值（指针保留 presence——
// inner_product 的距离是负内积，阈值 0/负值均为合法语义，不可用零哨兵）。
// EfSearch nil = 不注入（pgvector 缺省 40；B7 查询级调参，服务端校验
// [1,500]——指针保留 presence，缺省行为与未提供时逐字节一致）。
type VectorSearch struct {
	Attribute   string
	Values      []float64
	Metric      string
	MaxDistance *float64
	EfSearch    *int32
}

// Supported distance metrics（与 proto DistanceMetric / hnsw 索引同枚举域）。
const (
	MetricCosine       = "COSINE"
	MetricL2           = "L2"
	MetricInnerProduct = "INNER_PRODUCT"
)

// Query is the parsed AST: Filter tree + Orders + page.
// Parse / ParseMany are Appwrite-string codecs into this model.
// Filters is the implicit-AND leaf list produced by the codec (same as
// today's ParseMany); compilers should prefer Filter when set.
type Query struct {
	Filter       *Filter
	Filters      []Filter
	Orders       []Order
	Selects      []string
	VectorSearch *VectorSearch
	Limit        int
	Offset       int
	CursorAfter  string
	CursorBefore string
	PageSize     int32
	PageToken    string
}

// IsActive reports whether the AST carries filter, orders, or page data.
func (q *Query) IsActive() bool {
	if q == nil {
		return false
	}
	return q.Filter != nil || len(q.Filters) > 0 || len(q.Orders) > 0 ||
		len(q.Selects) > 0 || q.VectorSearch != nil || q.Limit != 0 || q.Offset != 0 ||
		q.CursorAfter != "" || q.CursorBefore != "" ||
		q.PageSize != 0 || q.PageToken != ""
}

// HasPredicate is true when a filter tree or codec leaf list is present.
func (q *Query) HasPredicate() bool {
	return q != nil && (q.Filter != nil || len(q.Filters) > 0)
}

// HasOrders is true when at least one order clause is present.
func (q *Query) HasOrders() bool {
	return q != nil && len(q.Orders) > 0
}

// HasPage is true when any page bound / token / cursor / limit / offset is set.
func (q *Query) HasPage() bool {
	if q == nil {
		return false
	}
	return q.PageSize != 0 || q.PageToken != "" || q.Limit != 0 || q.Offset != 0 ||
		q.CursorAfter != "" || q.CursorBefore != ""
}

// WalkLeaves visits comparison predicates in preorder (skips and/or).
func (q *Query) WalkLeaves(fn func(Filter)) {
	if q == nil || fn == nil {
		return
	}
	if q.Filter != nil {
		walkLeaves(q.Filter, fn)
		return
	}
	for _, f := range q.Filters {
		fn(f)
	}
}

func walkLeaves(f *Filter, fn func(Filter)) {
	if f == nil {
		return
	}
	switch f.Op {
	case OpAnd, OpOr:
		for _, c := range f.Children {
			walkLeaves(c, fn)
		}
	default:
		fn(*f)
	}
}

// Validate 检查比较叶必须带值，且叶数不超过 MaxQueries。
func (q *Query) Validate() error {
	if q == nil {
		return nil
	}
	n := 0
	var err error
	q.WalkLeaves(func(f Filter) {
		if err != nil {
			return
		}
		n++
		if n > MaxQueries {
			err = fmt.Errorf("filter node count exceeds maximum of %d", MaxQueries)
			return
		}
		err = validateLeaf(f)
	})
	return err
}

func validateLeaf(f Filter) error {
	switch f.Op {
	case OpIsNull, OpIsNotNull:
		return nil
	case OpBetween, OpNotBetween:
		if len(f.Values) != 2 {
			return fmt.Errorf("%s requires 2 values", f.Op)
		}
	default:
		if len(f.Values) < 1 {
			return fmt.Errorf("%s requires at least 1 value", f.Op)
		}
	}
	return nil
}

func andFilters(leaves []Filter) *Filter {
	switch len(leaves) {
	case 0:
		return nil
	case 1:
		f := leaves[0]
		return &f
	default:
		children := make([]*Filter, len(leaves))
		for i := range leaves {
			f := leaves[i]
			children[i] = &f
		}
		return &Filter{Op: OpAnd, Children: children}
	}
}

var queryRe = regexp.MustCompile(`^(\w+)\((.*)\)$`)

// Parse parses a single Appwrite-style query string into an AST Query.
// 定位（C7 单 AST）：本解析器是**客户端语法糖**——SDK/工具把 DSL 串解析成
// AST 后以 typed Query 发送；服务端文档查询栈不消费字符串。示例：
//
//	equal("email","a@b.com")
//	greaterThan("age",18)
//	contains("name","john")
//	orderDesc("createdAt")
//	limit(25)
func Parse(raw string) (*Query, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &Query{}, nil
	}
	if len(raw) > MaxQueryLen {
		return nil, fmt.Errorf("query string exceeds maximum length of %d", MaxQueryLen)
	}

	m := queryRe.FindStringSubmatch(raw)
	if m == nil {
		return nil, fmt.Errorf("invalid query format: %s", raw)
	}
	op := m[1]
	argStr := m[2]

	args, err := splitArgs(argStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse query %q: %w", raw, err)
	}

	switch op {
	case OpEqual, OpNotEqual, OpLessThan, OpLessThanEqual, OpGreaterThan, OpGreaterThanEqual,
		OpContains, OpNotContains, OpStartsWith, OpNotStartsWith, OpEndsWith, OpNotEndsWith,
		OpSearch, OpNotSearch:
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
		if len(values) < 1 {
			return nil, fmt.Errorf("%s requires at least 1 value", op)
		}
		leaf := Filter{Op: op, Attribute: attr, Values: values}
		return &Query{Filters: []Filter{leaf}, Filter: &leaf}, nil

	case OpIn:
		// Appwrite 语义：值必须是数组（in("status", ["a","b"])）。编译端
		// （compilePredicate 的 OpIn）与 proto AST codec（Filter_In）均已支持，
		// 此处补齐 DSL 解析使双栈算子对齐。
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
		leaf := Filter{Op: OpIn, Attribute: attr, Values: values}
		return &Query{Filters: []Filter{leaf}, Filter: &leaf}, nil

	case OpContainsAny, OpContainsAll:
		// 数组算子（§10.5 P0）：值必须是数组，与 in 同构
		// （containsAny("tags",["a","b"])）。
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
		leaf := Filter{Op: op, Attribute: attr, Values: values}
		return &Query{Filters: []Filter{leaf}, Filter: &leaf}, nil

	case OpBetween, OpNotBetween:
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
		leaf := Filter{Op: op, Attribute: attr, Values: []string{min, max}}
		return &Query{Filters: []Filter{leaf}, Filter: &leaf}, nil

	case OpIsNull, OpIsNotNull:
		if len(args) != 1 {
			return nil, fmt.Errorf("%s requires 1 arg", op)
		}
		attr, err := unquote(args[0])
		if err != nil {
			return nil, err
		}
		leaf := Filter{Op: op, Attribute: attr}
		return &Query{Filters: []Filter{leaf}, Filter: &leaf}, nil

	case "orderAsc", "orderDesc":
		if len(args) != 1 {
			return nil, fmt.Errorf("%s requires 1 arg", op)
		}
		attr, err := unquote(args[0])
		if err != nil {
			return nil, err
		}
		return &Query{Orders: []Order{{Attribute: attr, Desc: op == "orderDesc"}}}, nil

	case "limit":
		if len(args) != 1 {
			return nil, fmt.Errorf("limit requires 1 arg")
		}
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return nil, fmt.Errorf("limit must be an integer")
		}
		if n < 0 {
			return nil, fmt.Errorf("limit must be non-negative")
		}
		q := &Query{Limit: n}
		if n > 0 && n <= math.MaxInt32 {
			q.PageSize = int32(n)
		}
		return q, nil

	case "offset":
		if len(args) != 1 {
			return nil, fmt.Errorf("offset requires 1 arg")
		}
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return nil, fmt.Errorf("offset must be an integer")
		}
		if n < 0 {
			return nil, fmt.Errorf("offset must be non-negative")
		}
		return &Query{Offset: n}, nil

	case "cursorAfter", "cursorBefore":
		if len(args) != 1 {
			return nil, fmt.Errorf("%s requires 1 arg", op)
		}
		cursor, err := unquote(args[0])
		if err != nil {
			return nil, err
		}
		q := &Query{}
		if op == "cursorAfter" {
			q.CursorAfter = cursor
		} else {
			q.CursorBefore = cursor
		}
		return q, nil

	case "select":
		if len(args) != 1 {
			return nil, fmt.Errorf("select requires 1 arg")
		}
		fields, err := parseArray(args[0])
		if err != nil {
			return nil, err
		}
		return &Query{Selects: fields}, nil

	default:
		return nil, fmt.Errorf("unsupported query operator: %s", op)
	}
}

// ParseMany parses multiple Appwrite-style query strings and merges them into one Query.
// Filter predicates are combined with implicit AND (same as today's codec).
// 未显式指定 limit 时 Limit 保持 0，默认页大小由 adapter 决定（ListDocuments 用
// PageSize 回退），避免 DSL 层注入默认值掩盖调用方的分页参数。
func ParseMany(raw []string) (*Query, error) {
	if len(raw) > MaxQueries {
		return nil, fmt.Errorf("queries count exceeds maximum of %d", MaxQueries)
	}
	merged := &Query{}
	for _, r := range raw {
		if len(r) > MaxQueryLen {
			return nil, fmt.Errorf("query string exceeds maximum length of %d", MaxQueryLen)
		}
		q, err := Parse(r)
		if err != nil {
			return nil, err
		}
		merged.Filters = append(merged.Filters, q.Filters...)
		merged.Orders = append(merged.Orders, q.Orders...)
		merged.Selects = append(merged.Selects, q.Selects...)
		if q.Limit != 0 {
			merged.Limit = q.Limit
		}
		if q.PageSize != 0 {
			merged.PageSize = q.PageSize
		}
		if q.Offset != 0 {
			merged.Offset = q.Offset
		}
		if q.CursorAfter != "" {
			merged.CursorAfter = q.CursorAfter
		}
		if q.CursorBefore != "" {
			merged.CursorBefore = q.CursorBefore
		}
	}
	merged.Filter = andFilters(merged.Filters)
	return merged, nil
}

// splitArgs splits top-level arguments, respecting quoted strings and brackets.
func splitArgs(s string) ([]string, error) {
	var args []string
	var sb strings.Builder
	depth := 0
	inQuote := false
	var escape bool
	for i, r := range s {
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
		_ = i
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
	return unescapeString(s[1 : len(s)-1]), nil
}

func unquoteOrLiteral(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return unescapeString(s[1 : len(s)-1]), nil
	}
	return s, nil
}

// unescapeString reverses the escaping performed by escapeString.
func unescapeString(s string) string {
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

// escapeString escapes characters that would break the DSL grammar.
// Use this when composing query strings programmatically.
func escapeString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// quoteString wraps a value in double quotes and escapes inner quotes/backslashes.
func quoteString(s string) string {
	return `"` + escapeString(s) + `"`
}

// BuildFilter constructs a single Appwrite-style query string from structured args.
// It is the safe counterpart to Sprintf-based query construction: values are
// escaped so that quotes/backslashes inside user input cannot break out of the
// quoted scope.
func BuildFilter(op, attr string, values ...string) string {
	parts := make([]string, 0, len(values)+1)
	parts = append(parts, quoteString(attr))
	for _, v := range values {
		parts = append(parts, quoteString(v))
	}
	return op + "(" + strings.Join(parts, ",") + ")"
}

// BuildEqual is a shorthand for BuildFilter("equal", attr, values...).
func BuildEqual(attr string, values ...string) string {
	return BuildFilter("equal", attr, values...)
}

// BuildLimit constructs a limit(n) query string.
func BuildLimit(n int) string {
	return fmt.Sprintf("limit(%d)", n)
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
