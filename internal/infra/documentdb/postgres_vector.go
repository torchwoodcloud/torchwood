// vector 列编解码与清单缓存（会话 #10 §10.5 P0 最后一项）。
//
// 物理形态：pgvector 原生 VECTOR(dims) 列（扩展由迁移 000005 启用）。
// 写入：data 通道的 JSON 数组（[]any 浮点）编码为 pgvector 字面量
// "[1,2,3]" + ?::vector 绑定（pgdriver 无 vector 原生驱动，文本协议）。
// 读回：to_jsonb(vector) 输出字符串（原型 3 实证），Data 契约 = JSON
// 数组——读回投影以 to_jsonb(d.*) || jsonb_build_object(...::text::jsonb)
// 逐列覆盖（vector 文本形态即合法 JSON 数组文本）。
//
// vectorColumns 缓存（key = "schema.physical" → attr key → dims）：写/读
// 路径需要列类型信息而多数调用点无 coll 上下文（SystemPrincipal 读回、
// upsert 更新支），按物理表名缓存，miss 时点查 catalog attrs JSON。DDL
// 路径（CreateCollection/CreateAttribute/DeleteCollection）维护失效。
package documentdb

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
)

// vectorColumnsOf 返回集合的 vector 列清单（attr key → dims）。cache miss
// 时点查 catalog_collections.attrs（一次后缓存；空 map 表示无 vector 列，
// 避免重复查询）。返回空 map 表示无 vector 列（含系统集合——静态表不可能
// 声明 vector 属性）。
func (p *postgresDocumentDB) vectorColumnsOf(ctx context.Context, projectID, databaseID, collectionID, schema, physical string) (map[string]int, error) {
	key := schema + "." + physical
	if cached, ok := p.vectorColumns.Load(key); ok {
		cols, _ := cached.(map[string]int)
		return cols, nil
	}
	var attrsJSON string
	err := p.conn(ctx).NewSelect().Model((*model.DocumentCollection)(nil)).
		Column("attrs").
		Where("project_id = ? AND database_id = ? AND collection_id = ?", projectID, databaseID, collectionID).
		Scan(ctx, &attrsJSON)
	if err != nil {
		return nil, err
	}
	attrs, err := decodeAttributes(attrsJSON)
	if err != nil {
		return nil, err
	}
	cols := map[string]int{}
	for _, a := range attrs {
		if strings.ToLower(a.Type) == "vector" {
			cols[a.Key] = a.Dims
		}
	}
	p.vectorColumns.Store(key, cols)
	return cols, nil
}

// storeVectorColumns 在 DDL 成功后刷新缓存（CreateCollection/CreateAttribute）。
func (p *postgresDocumentDB) storeVectorColumns(schema, physical string, attrs []databases.Attribute) {
	cols := map[string]int{}
	for _, a := range attrs {
		if strings.ToLower(a.Type) == "vector" {
			cols[a.Key] = a.Dims
		}
	}
	p.vectorColumns.Store(schema+"."+physical, cols)
}

// dropVectorColumns 在 DeleteCollection 后清缓存（重建同名物理表概率可忽略，
// 清理仅为不残留陈旧维度）。
func (p *postgresDocumentDB) dropVectorColumns(schema, physical string) {
	p.vectorColumns.Delete(schema + "." + physical)
}

// pgVectorLiteral 把 data 通道的向量值（[]any 浮点/整数）编码为 pgvector
// 字面量 "[1,2,3]"。非数组或含非数值元素返回 false（调用方按 InvalidArgument
// 拒绝——vector 列只接受数值数组）。
func pgVectorLiteral(v any) (string, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(arr))
	for _, e := range arr {
		switch ev := e.(type) {
		case float64:
			parts = append(parts, strconv.FormatFloat(ev, 'g', -1, 64))
		case int:
			parts = append(parts, strconv.Itoa(ev))
		case int64:
			parts = append(parts, strconv.FormatInt(ev, 10))
		default:
			return "", false
		}
	}
	return "[" + strings.Join(parts, ",") + "]", true
}

// validateVectorValue 校验向量写入值：必须是数值数组且维度与声明一致。
// （PG VECTOR(dims) 列本身会拒绝维度不匹配，但错误是 22P02 类运行时错误；
// 在绑定前显式校验给出 InvalidArgument 语义。）
func validateVectorValue(key string, v any, dims int) error {
	arr, ok := v.([]any)
	if !ok {
		return status.Error(codes.InvalidArgument, fmt.Sprintf(
			"attribute %q: vector value must be a JSON array of numbers", key))
	}
	if len(arr) != dims {
		return status.Error(codes.InvalidArgument, fmt.Sprintf(
			"attribute %q: vector value has %d dimensions, expected %d", key, len(arr), dims))
	}
	for _, e := range arr {
		switch e.(type) {
		case float64, int, int64:
		default:
			return status.Error(codes.InvalidArgument, fmt.Sprintf(
				"attribute %q: vector value must contain only numbers", key))
		}
	}
	return nil
}

// vectorProjection 生成读回投影：无 vector 列时为 to_jsonb(d.*)；有则逐列
// 覆盖为 ::text::jsonb（原型 3：vector 文本形态即合法 JSON 数组文本）。
// 覆盖发生在 to_jsonb 之后，其余列（系统列/_acl/普通属性）不受影响。
func vectorProjection(vectorCols map[string]int) string {
	if len(vectorCols) == 0 {
		return "to_jsonb(d.*)"
	}
	parts := make([]string, 0, len(vectorCols))
	for key := range vectorCols {
		parts = append(parts, fmt.Sprintf("'%s', d.%s::text::jsonb", escapeSQLStringLiteral(key), quoteIdent(key)))
	}
	return "to_jsonb(d.*) || jsonb_build_object(" + strings.Join(parts, ", ") + ")"
}

// distanceOp 映射 metric 到 pgvector KNN 操作符（会话 #10 预决策 3）：
// cosine <=> / L2 <-> / inner <#>（pgvector 以负内积为"距离"，值越小越近）。
func distanceOp(metric string) (string, error) {
	switch normalizeMetric(metric) {
	case MetricCosineName:
		return "<=>", nil
	case "L2":
		return "<->", nil
	case "INNER_PRODUCT":
		return "<#>", nil
	default:
		return "", status.Error(codes.InvalidArgument,
			fmt.Sprintf("distance metric must be COSINE, L2, or INNER_PRODUCT, got %q", metric))
	}
}

// MetricCosineName 是归一后的余弦度量名（与 pkg/query.MetricCosine 同值；
// 独立常量避免 documentdb 反向依赖 pkg/query 的约定漂移）。
const MetricCosineName = "COSINE"

// pgVectorFloatLiteral 把 KNN 查询向量编码为 pgvector 字面量（绑定参数）。
func pgVectorFloatLiteral(values []float64) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.FormatFloat(v, 'g', -1, 64))
	}
	return "[" + strings.Join(parts, ",") + "]"
}
