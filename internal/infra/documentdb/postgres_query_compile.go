// Appwrite DSL → SQL 编译：谓词编译、字段白/黑名单校验、输入上限、LIKE 转义。
package documentdb

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// escapeLikePattern 转义 ILIKE 模式中的通配符与转义符本身（配合 ESCAPE '\' 子句），
// 使 contains/startsWith/endsWith 将用户输入按字面量匹配。
func escapeLikePattern(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func mapQueryField(field string) string {
	switch field {
	case "$id", "_id":
		return "_id"
	case "$createdAt", "_created_at":
		return "_created_at"
	case "$updatedAt", "_updated_at":
		return "_updated_at"
	case "$version", "_version":
		return "_version"
	}
	return field
}

// systemQueryFields 是查询白名单中的系统列（含 $ 别名映射后的内部名）。
var systemQueryFields = []string{"_id", "_created_at", "_updated_at", "_version"}

// sensitiveQueryFields 是 default 库系统集合中禁止作为查询过滤/排序条件的
// 凭据/令牌类列（任何角色不得探测）；仅按 databases.IsSystemCollection 限定，
// 自定义库同名集合不受影响。phone 等 PII 管理列按 D4 决策保留可查。
var sensitiveQueryFields = map[string]map[string]struct{}{
	"users":      {"password_hash": {}, "prefs": {}, "labels": {}},
	"sessions":   {"secret_hash": {}},
	"identities": {"provider_data": {}},
}

// arrayTypesOf 抽取集合声明属性中的数组列（key → PG 数组类型），供查询编译
// 的数组算子（&& / @>）与白名单校验单源使用（阶段③-b 预决策 2）。
func arrayTypesOf(coll *databases.Collection) map[string]string {
	if coll == nil {
		return nil
	}
	var out map[string]string
	for _, attr := range coll.Attributes {
		if attr.Array {
			if out == nil {
				out = make(map[string]string, len(coll.Attributes))
			}
			out[attr.Key] = pgArrayTypeFor(attr.Type)
		}
	}
	return out
}

// vectorColumnsFromColl 抽取集合声明属性中的 vector 列（key → dims）。
func vectorColumnsFromColl(coll *databases.Collection) map[string]int {
	if coll == nil {
		return nil
	}
	var out map[string]int
	for _, attr := range coll.Attributes {
		if strings.ToLower(attr.Type) == "vector" {
			if out == nil {
				out = make(map[string]int, len(coll.Attributes))
			}
			out[attr.Key] = attr.Dims
		}
	}
	return out
}

// validateVectorSearch 是 KNN 算子的前置校验（会话 #10 预决策 3，显式拒绝
// 原则）：目标列必须是声明的 vector 属性（维度等长）、且存在与请求 metric
// 匹配的 hnsw 索引（无索引/metric 不符 → InvalidArgument——search 需
// fulltext 索引的同款纪律）。ef_search（B7）合法域 [1,500]：≤0 非法
// （pgvector 要求 ≥1），>500 超防滥用上限——一律 InvalidArgument 显式拒绝
// （不用静默 clamp：R9 显式拒绝原则，静默改写让调用方误以为请求值生效）。
func validateVectorSearch(coll *databases.Collection, vs *query.VectorSearch) error {
	if vs == nil {
		return nil
	}
	if vs.EfSearch != nil {
		ef := *vs.EfSearch
		if ef < 1 {
			return status.Error(codes.InvalidArgument,
				fmt.Sprintf("vector_search ef_search must be >= 1, got %d", ef))
		}
		if ef > maxEfSearch {
			return status.Errorf(codes.InvalidArgument,
				"vector_search ef_search %d exceeds maximum of %d", ef, maxEfSearch)
		}
	}
	dims, ok := vectorColumnsFromColl(coll)[vs.Attribute]
	if !ok {
		return status.Error(codes.InvalidArgument,
			fmt.Sprintf("vector_search requires a vector attribute: %s", vs.Attribute))
	}
	if len(vs.Values) != dims {
		return status.Error(codes.InvalidArgument,
			fmt.Sprintf("vector_search on %s has %d dimensions, expected %d", vs.Attribute, len(vs.Values), dims))
	}
	for _, idx := range coll.Indexes {
		if !strings.EqualFold(idx.Type, "hnsw") || len(idx.Attributes) == 0 || idx.Attributes[0] != vs.Attribute {
			continue
		}
		if strings.EqualFold(normalizeMetric(idx.DistanceMetric), vs.Metric) {
			return nil
		}
	}
	return status.Error(codes.InvalidArgument,
		fmt.Sprintf("vector_search with metric %s requires an hnsw index on %s with the same distance_metric", vs.Metric, vs.Attribute))
}

// normalizeMetric 把 metric 归一为大写（缺省 COSINE——catalog 落库已归一，
// 此处兼容直写 catalog 的存量小写形态）。
func normalizeMetric(m string) string {
	if strings.ToUpper(m) == "" {
		return "COSINE"
	}
	return strings.ToUpper(m)
}

// validateQueryFields 校验非 System 查询路径（A7）：Filters/Orders/Selects 字段
// 白名单（系统列 + 声明 attrs）、敏感列黑名单、search 的 fulltext 索引约束、
// containsAny/containsAll 的数组列约束（阶段③-b：仅 array=true 属性可用，
// 标量列/系统列拒绝）。
// _version 特判：系统集合拒绝（无此列）；用户集合列尚未 reconcile（缺列）时返回
// version_column_unavailable，不得落 PG 未定义列错误（读路径不 ALTER）。
// 物理表名（physical）供 _version readiness 检查；collectionID 保留逻辑名供
// 系统集合敏感列黑名单。SystemPrincipal 路径不调用本函数（信任内部调用，
// 零额外元数据查询）。
func (p *postgresDocumentDB) validateQueryFields(ctx context.Context, schema, physical string, parsed *query.Query, coll *databases.Collection, collectionID string, isSystem bool) error {
	allowed := make(map[string]struct{}, len(systemQueryFields)+len(coll.Attributes))
	for _, f := range systemQueryFields {
		allowed[f] = struct{}{}
	}
	for _, attr := range coll.Attributes {
		allowed[attr.Key] = struct{}{}
	}
	// deprecated 属性集（B4 读屏蔽：查询白名单拒绝）。
	deprecatedAttrs := map[string]bool{}
	for _, attr := range coll.Attributes {
		if attr.StatusOrDefault() == databases.AttrStatusDeprecated {
			deprecatedAttrs[attr.Key] = true
		}
	}
	fulltextAttrs := map[string]struct{}{}
	for _, idx := range coll.Indexes {
		if strings.ToLower(idx.Type) == "fulltext" {
			for _, a := range idx.Attributes {
				fulltextAttrs[a] = struct{}{}
			}
		}
	}
	arrayTypes := arrayTypesOf(coll)
	vectorTypes := vectorColumnsFromColl(coll)
	if err := validateVectorSearch(coll, parsed.VectorSearch); err != nil {
		return err
	}

	checkField := func(name string) error {
		field := mapQueryField(name)
		if _, ok := allowed[field]; !ok {
			return status.Error(codes.InvalidArgument, fmt.Sprintf("invalid query field: %s", name))
		}
		// B4 生命周期：deprecated 属性读屏蔽——查询白名单拒绝（数据仍在，
		// RestoreAttribute 可回滚）。migrating 属性查询放行（读服务旧列）。
		if st, ok := deprecatedAttrs[mapQueryField(name)]; ok && st {
			return status.Errorf(codes.InvalidArgument, "attribute %q is deprecated and not queryable", name)
		}
		if field == "_version" {
			if isSystem {
				// 系统表无 _version 列，禁止编进 SQL。
				return status.Error(codes.InvalidArgument, fmt.Sprintf("invalid query field: %s", name))
			}
			// 用户集合：列尚未 reconcile → version_column_unavailable；
			// 列已存在但非 bigint → version_column_conflict。
			// 均不落 PG 42703；不得改写成对常量 1 的比较（equal("$version", 2) 会静默语义错误）。
			ready, err := p.versionColumnReady(ctx, schema, physical)
			if err != nil {
				return err
			}
			if !ready {
				return status.Error(codes.InvalidArgument, databases.ErrVersionColumnUnavailable.Error())
			}
		}
		if isSystem {
			if sensitive, ok := sensitiveQueryFields[collectionID]; ok {
				if _, blocked := sensitive[field]; blocked {
					return status.Error(codes.InvalidArgument, fmt.Sprintf("field is not queryable: %s", name))
				}
			}
		}
		return nil
	}

	var fieldErr error
	parsed.WalkLeaves(func(f query.Filter) {
		if fieldErr != nil {
			return
		}
		if err := checkField(f.Attribute); err != nil {
			fieldErr = err
			return
		}
		// vector 属性（会话 #10 预决策 3）：距离不可作布尔谓词——普通 filter
		// 仅放行 isNull/isNotNull；其余算子（eq/lt/contains...）对 vector 列
		// 无可编译语义，显式拒绝。
		if _, isVec := vectorTypes[mapQueryField(f.Attribute)]; isVec {
			if f.Op != query.OpIsNull && f.Op != query.OpIsNotNull {
				fieldErr = status.Errorf(codes.InvalidArgument,
					"vector attribute %s only supports isNull/isNotNull filters; use vector_search for KNN", f.Attribute)
				return
			}
		}
		if f.Op == query.OpSearch {
			field := mapQueryField(f.Attribute)
			if _, ok := fulltextAttrs[field]; !ok {
				fieldErr = status.Error(codes.InvalidArgument, fmt.Sprintf("search requires a fulltext index on: %s", f.Attribute))
			}
			return
		}
		if f.Op == query.OpContainsAny || f.Op == query.OpContainsAll {
			field := mapQueryField(f.Attribute)
			if _, ok := arrayTypes[field]; !ok {
				fieldErr = status.Errorf(codes.InvalidArgument, "%s requires an array attribute: %s", f.Op, f.Attribute)
			}
		}
	})
	if fieldErr != nil {
		return fieldErr
	}
	for _, o := range parsed.Orders {
		if err := checkField(o.Attribute); err != nil {
			return err
		}
		if _, isVec := vectorTypes[mapQueryField(o.Attribute)]; isVec {
			return status.Errorf(codes.InvalidArgument,
				"vector attribute %s cannot be an order key; vector_search carries the ordering", o.Attribute)
		}
	}
	for _, s := range parsed.Selects {
		if err := checkField(s); err != nil {
			return err
		}
	}
	return nil
}

// astFrom 是 SQL 编译前的 AST 入口（C7 单 AST）：caller（app 层
// ResolveQuery）已填好 AST，此处合并 GET 面分页字段后校验。服务端不再
// 解析 DSL 字符串（queries 回退分支已随双栈退役删除）。
func astFrom(q databases.Query) (*query.Query, error) {
	ast := cloneQuery(q.AST)
	if ast.PageSize == 0 {
		ast.PageSize = q.PageSize
	}
	if ast.PageToken == "" {
		ast.PageToken = q.PageToken
	}
	if err := ast.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid query: %v", err)
	}
	return ast, nil
}

func cloneQuery(src *query.Query) *query.Query {
	if src == nil {
		return &query.Query{}
	}
	cp := *src
	return &cp
}

// buildAppwriteQuery 编译过滤树与排序。arrayTypes（key → PG 数组类型）供
// containsAny/containsAll 编译 && / @> 与元素类型 cast（阶段③-b 预决策 2）。
func buildAppwriteQuery(parsed *query.Query, arrayTypes map[string]string) (string, []any, string, error) {
	var where string
	var args []any
	var err error
	if parsed.Filter != nil {
		where, args, err = compileFilter(parsed.Filter, arrayTypes)
	} else if len(parsed.Filters) > 0 {
		children := make([]*query.Filter, len(parsed.Filters))
		for i := range parsed.Filters {
			f := parsed.Filters[i]
			children[i] = &f
		}
		where, args, err = compileBool(children, "AND", arrayTypes)
	}
	if err != nil {
		return "", nil, "", err
	}
	// 跨 filter 绑定参数累计上限：单 filter 已限 maxFilterValues，但 100 条
	// query × 1000 值可累积 10 万绑定参数，超出 PG 单语句 65535 参数上限后
	// 以运行时错误暴露。此处封死总量（List/Count/Sum 共用）。
	if len(args) > maxTotalFilterParams {
		return "", nil, "", status.Errorf(codes.InvalidArgument,
			"query filters bind %d parameters in total, exceeds maximum of %d", len(args), maxTotalFilterParams)
	}

	// R08-P2-4：默认排序带 _id tiebreaker，同 _created_at 的多行分页保持稳定。
	orderSQL := "ORDER BY d._created_at DESC, d._id DESC"
	if len(parsed.Orders) > 0 {
		var parts []string
		for _, o := range parsed.Orders {
			field := mapQueryField(o.Attribute)
			if !safeNameRe.MatchString(field) {
				return "", nil, "", status.Error(codes.InvalidArgument, fmt.Sprintf("invalid order field: %s", o.Attribute))
			}
			dir := "ASC"
			if o.Desc {
				dir = "DESC"
			}
			parts = append(parts, fmt.Sprintf("d.%s %s", quoteIdent(field), dir))
		}
		if len(parts) > 0 {
			// 与 cursor 续页路径（postgres_document_query.go 的 ORDER BY / keyset
			// 谓词 `<field>, d._id` 二元组）同构：分页各页必须同一全序，否则同键
			// 多行跨页丢/重。不补 _created_at 中段——那会使首页与 cursor 页序不同
			// 构（R08-P2-4 的遗留缺陷）。_id 方向跟随首个排序键（与 cursor 路径一致）。
			dir := "ASC"
			if parsed.Orders[0].Desc {
				dir = "DESC"
			}
			orderSQL = "ORDER BY " + strings.Join(parts, ", ") + ", d._id " + dir
		}
	}
	return where, args, orderSQL, nil
}

func compileFilter(f *query.Filter, arrayTypes map[string]string) (string, []any, error) {
	if f == nil {
		return "", nil, nil
	}
	switch f.Op {
	case query.OpAnd:
		return compileBool(f.Children, "AND", arrayTypes)
	case query.OpOr:
		return compileBool(f.Children, "OR", arrayTypes)
	default:
		return compilePredicate(f, arrayTypes)
	}
}

func compileBool(children []*query.Filter, join string, arrayTypes map[string]string) (string, []any, error) {
	var parts []string
	var args []any
	for _, c := range children {
		if c == nil {
			continue
		}
		w, a, err := compileFilter(c, arrayTypes)
		if err != nil {
			return "", nil, err
		}
		if w == "" {
			continue
		}
		parts = append(parts, w)
		args = append(args, a...)
	}
	if len(parts) == 0 {
		return "", nil, nil
	}
	if len(parts) == 1 {
		return parts[0], args, nil
	}
	return "(" + strings.Join(parts, " "+join+" ") + ")", args, nil
}

func compilePredicate(f *query.Filter, arrayTypes map[string]string) (string, []any, error) {
	field := mapQueryField(f.Attribute)
	if !safeNameRe.MatchString(field) {
		return "", nil, fmt.Errorf("invalid query field: %s", f.Attribute)
	}
	col := "d." + quoteIdent(field)
	switch f.Op {
	case query.OpEqual, query.OpIn:
		if len(f.Values) < 1 {
			return "", nil, status.Errorf(codes.InvalidArgument, "%s requires at least 1 value", f.Op)
		}
		if len(f.Values) > maxFilterValues {
			return "", nil, status.Error(codes.InvalidArgument, fmt.Sprintf("filter values exceed maximum of %d", maxFilterValues))
		}
		if len(f.Values) == 1 {
			return fmt.Sprintf("%s = ?", col), []any{f.Values[0]}, nil
		}
		phs := strings.TrimSuffix(strings.Repeat("?, ", len(f.Values)), ", ")
		args := make([]any, len(f.Values))
		for i, v := range f.Values {
			args[i] = v
		}
		return fmt.Sprintf("%s IN (%s)", col, phs), args, nil
	case query.OpNotEqual:
		if len(f.Values) < 1 {
			return "", nil, status.Errorf(codes.InvalidArgument, "%s requires at least 1 value", f.Op)
		}
		if len(f.Values) > maxFilterValues {
			return "", nil, status.Error(codes.InvalidArgument, fmt.Sprintf("filter values exceed maximum of %d", maxFilterValues))
		}
		if len(f.Values) == 1 {
			return fmt.Sprintf("%s != ?", col), []any{f.Values[0]}, nil
		}
		phs := strings.TrimSuffix(strings.Repeat("?, ", len(f.Values)), ", ")
		args := make([]any, len(f.Values))
		for i, v := range f.Values {
			args[i] = v
		}
		return fmt.Sprintf("%s NOT IN (%s)", col, phs), args, nil
	case query.OpLessThan, query.OpLessThanEqual, query.OpGreaterThan, query.OpGreaterThanEqual,
		query.OpContains, query.OpStartsWith, query.OpEndsWith, query.OpSearch,
		query.OpNotContains, query.OpNotStartsWith, query.OpNotEndsWith, query.OpNotSearch:
		if len(f.Values) < 1 {
			return "", nil, status.Errorf(codes.InvalidArgument, "%s requires at least 1 value", f.Op)
		}
		switch f.Op {
		case query.OpLessThan:
			return fmt.Sprintf("%s < ?", col), []any{f.Values[0]}, nil
		case query.OpLessThanEqual:
			return fmt.Sprintf("%s <= ?", col), []any{f.Values[0]}, nil
		case query.OpGreaterThan:
			return fmt.Sprintf("%s > ?", col), []any{f.Values[0]}, nil
		case query.OpGreaterThanEqual:
			return fmt.Sprintf("%s >= ?", col), []any{f.Values[0]}, nil
		case query.OpContains:
			return fmt.Sprintf(`%s ILIKE ? ESCAPE '\'`, col), []any{"%" + escapeLikePattern(f.Values[0]) + "%"}, nil
		case query.OpStartsWith:
			return fmt.Sprintf(`%s ILIKE ? ESCAPE '\'`, col), []any{escapeLikePattern(f.Values[0]) + "%"}, nil
		case query.OpEndsWith:
			return fmt.Sprintf(`%s ILIKE ? ESCAPE '\'`, col), []any{"%" + escapeLikePattern(f.Values[0])}, nil
		case query.OpSearch:
			return fmt.Sprintf("to_tsvector('simple', %s::text) @@ plainto_tsquery('simple', ?)", col), []any{f.Values[0]}, nil
		// not* 变体（C7 预决策 1）：NOT 包裹正算子。三值逻辑下 NULL 键行
		// 对 NOT(比较) 求值为 NULL 而被排除——与 Appwrite/SQL NOT 语义一致。
		case query.OpNotContains:
			return fmt.Sprintf(`%s NOT ILIKE ? ESCAPE '\'`, col), []any{"%" + escapeLikePattern(f.Values[0]) + "%"}, nil
		case query.OpNotStartsWith:
			return fmt.Sprintf(`%s NOT ILIKE ? ESCAPE '\'`, col), []any{escapeLikePattern(f.Values[0]) + "%"}, nil
		case query.OpNotEndsWith:
			return fmt.Sprintf(`%s NOT ILIKE ? ESCAPE '\'`, col), []any{"%" + escapeLikePattern(f.Values[0])}, nil
		default:
			return fmt.Sprintf("NOT (to_tsvector('simple', %s::text) @@ plainto_tsquery('simple', ?))", col), []any{f.Values[0]}, nil
		}
	case query.OpIsNull:
		return fmt.Sprintf("%s IS NULL", col), nil, nil
	case query.OpIsNotNull:
		return fmt.Sprintf("%s IS NOT NULL", col), nil, nil
	case query.OpContainsAny, query.OpContainsAll:
		// 数组算子（阶段③-b 预决策 2）：containsAny → &&（交集非空），
		// containsAll → @>（子集）。参数按列元素类型显式 cast（pgTextArray
		// 字面量 + ?::T[]）；arrayTypes 缺席（标量列/系统列）由
		// validateQueryFields 白名单先行拒绝，此处兜底 fail-closed。
		if len(f.Values) < 1 {
			return "", nil, status.Errorf(codes.InvalidArgument, "%s requires at least 1 value", f.Op)
		}
		if len(f.Values) > maxFilterValues {
			return "", nil, status.Error(codes.InvalidArgument, fmt.Sprintf("filter values exceed maximum of %d", maxFilterValues))
		}
		arrType, ok := arrayTypes[field]
		if !ok {
			return "", nil, status.Errorf(codes.InvalidArgument, "%s requires an array attribute: %s", f.Op, f.Attribute)
		}
		op := "&&"
		if f.Op == query.OpContainsAll {
			op = "@>"
		}
		return fmt.Sprintf("%s %s ?::%s", col, op, arrType), []any{pgTextArray(f.Values)}, nil
	case query.OpBetween:
		if len(f.Values) != 2 {
			return "", nil, fmt.Errorf("between requires 2 values")
		}
		return fmt.Sprintf("%s BETWEEN ? AND ?", col), []any{f.Values[0], f.Values[1]}, nil
	case query.OpNotBetween:
		if len(f.Values) != 2 {
			return "", nil, fmt.Errorf("notBetween requires 2 values")
		}
		return fmt.Sprintf("%s NOT BETWEEN ? AND ?", col), []any{f.Values[0], f.Values[1]}, nil
	default:
		return "", nil, fmt.Errorf("unsupported filter operator: %s", f.Op)
	}
}
