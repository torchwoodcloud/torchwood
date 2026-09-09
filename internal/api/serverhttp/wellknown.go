package serverhttp

import (
	"encoding/json"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// /.well-known/torchwood（B10，redesign §4.1 Agent 面）：可发现性目录——
// 纯 HTTP 面静态路由（无 gRPC 对应物，走 gateway mux HandlePath 注册形态，
// 与 storage/OAuth multipart 自定义 handler 同机制）。
//
// 内容三段：
//   - query.operators：文档查询算子全集（canonical AST 名 = Appwrite DSL 名，
//     附 proto Filter oneof 字段与值数量约束；containsAny/containsAll 标注
//     array_only）——与 pkg/query 常量、proto oneof 的同步由 wellknown_test
//     防漂移断言锁定；
//   - error_codes：域码表 + retryable——单一事实源为 databases.ErrorCodeCatalog()
//     （构造期直读，天然同步；app/shared domainCodeGRPC 与其双向一致另有测试）；
//   - resources.databases.verbs：Server API databases 面动词清单（rpc + HTTP
//     形态 + scope）；scope 取自 auth.APIKeyScopeRules()（单一事实源）。
//
// 目录为手写结构 + 测试断言与源同步（B10 判据明确不要求运行时反射；scope 与
// 错误码两段例外——源本身有导出访问器，构造期直取零漂移）。

// wellKnownQueryOperator 是目录中单个查询算子的条目。
type wellKnownQueryOperator struct {
	Name       string `json:"name"`                 // canonical AST / Appwrite DSL 名
	ProtoField string `json:"proto_field"`          // shared.v1.Filter oneof expr 字段
	MinValues  int    `json:"min_values,omitempty"` // Comparison.values 数量下限
	MaxValues  int    `json:"max_values,omitempty"` // 数量上限；0 = 无上限
	Kind       string `json:"kind"`                 // comparison | combinator
	ArrayOnly  bool   `json:"array_only,omitempty"` // 仅 array=true 属性可用
}

// wellKnownVerb 是 databases 面单个动词的条目。
type wellKnownVerb struct {
	RPC   string `json:"rpc"`   // DatabasesService 方法名
	HTTP  string `json:"http"`  // REST 形态（多绑定为 "A; B"）
	Scope string `json:"scope"` // API key scope（<resource>.<read|write>）
}

// wellKnownQueryOpIndex 是 operators 的有序清单（目录输出顺序稳定）。
var wellKnownQueryOps = []wellKnownQueryOperator{
	{Name: query.OpEqual, ProtoField: "eq", MinValues: 1, Kind: "comparison"},
	{Name: query.OpNotEqual, ProtoField: "ne", MinValues: 1, Kind: "comparison"},
	{Name: query.OpLessThan, ProtoField: "lt", MinValues: 1, Kind: "comparison"},
	{Name: query.OpLessThanEqual, ProtoField: "lte", MinValues: 1, Kind: "comparison"},
	{Name: query.OpGreaterThan, ProtoField: "gt", MinValues: 1, Kind: "comparison"},
	{Name: query.OpGreaterThanEqual, ProtoField: "gte", MinValues: 1, Kind: "comparison"},
	{Name: query.OpIn, ProtoField: "in", MinValues: 1, Kind: "comparison"},
	{Name: query.OpContains, ProtoField: "contains", MinValues: 1, Kind: "comparison"},
	{Name: query.OpNotContains, ProtoField: "not_contains", MinValues: 1, Kind: "comparison"},
	{Name: query.OpStartsWith, ProtoField: "starts_with", MinValues: 1, Kind: "comparison"},
	{Name: query.OpNotStartsWith, ProtoField: "not_starts_with", MinValues: 1, Kind: "comparison"},
	{Name: query.OpEndsWith, ProtoField: "ends_with", MinValues: 1, Kind: "comparison"},
	{Name: query.OpNotEndsWith, ProtoField: "not_ends_with", MinValues: 1, Kind: "comparison"},
	{Name: query.OpSearch, ProtoField: "search", MinValues: 1, Kind: "comparison"},
	{Name: query.OpNotSearch, ProtoField: "not_search", MinValues: 1, Kind: "comparison"},
	{Name: query.OpIsNull, ProtoField: "is_null", Kind: "comparison"},
	{Name: query.OpIsNotNull, ProtoField: "is_not_null", Kind: "comparison"},
	{Name: query.OpBetween, ProtoField: "between", MinValues: 2, MaxValues: 2, Kind: "comparison"},
	{Name: query.OpNotBetween, ProtoField: "not_between", MinValues: 2, MaxValues: 2, Kind: "comparison"},
	{Name: query.OpContainsAny, ProtoField: "contains_any", MinValues: 1, Kind: "comparison", ArrayOnly: true},
	{Name: query.OpContainsAll, ProtoField: "contains_all", MinValues: 1, Kind: "comparison", ArrayOnly: true},
	{Name: query.OpAnd, ProtoField: "and", Kind: "combinator"},
	{Name: query.OpOr, ProtoField: "or", Kind: "combinator"},
}

// wellKnownVerbs 是 Server API databases 面动词清单（B10：资源目录）。
var wellKnownVerbs = []wellKnownVerb{
	{RPC: "CreateDatabase", HTTP: "POST /v1/server/databases"},
	{RPC: "ListDatabases", HTTP: "GET /v1/server/databases"},
	{RPC: "GetDatabase", HTTP: "GET /v1/server/databases/{id}"},
	{RPC: "DeleteDatabase", HTTP: "DELETE /v1/server/databases/{id}"},
	{RPC: "CreateCollection", HTTP: "POST /v1/server/databases/{database_id}/collections"},
	{RPC: "ListCollections", HTTP: "GET /v1/server/databases/{database_id}/collections"},
	{RPC: "GetCollection", HTTP: "GET /v1/server/databases/{database_id}/collections/{collection_id}"},
	{RPC: "DeleteCollection", HTTP: "DELETE /v1/server/databases/{database_id}/collections/{collection_id}"},
	{RPC: "UpdateCollection", HTTP: "PATCH /v1/server/databases/{database_id}/collections/{collection_id}"},
	{RPC: "ExportCollectionSchema", HTTP: "GET /v1/server/databases/{database_id}/collections/{collection_id}:exportSchema"},
	{RPC: "CreateAttribute", HTTP: "POST /v1/server/databases/{database_id}/collections/{collection_id}/attributes"},
	{RPC: "DeleteAttribute", HTTP: "DELETE /v1/server/databases/{database_id}/collections/{collection_id}/attributes/{key}"},
	// B4 schema 演进生命周期（§4.6）：回滚 / 删列段二 / copy 迁移。
	{RPC: "RestoreAttribute", HTTP: "POST /v1/server/databases/{database_id}/collections/{collection_id}/attributes/{key}:restore"},
	{RPC: "RetireAttribute", HTTP: "POST /v1/server/databases/{database_id}/collections/{collection_id}/attributes/{key}:retire"},
	{RPC: "MigrateAttribute", HTTP: "POST /v1/server/databases/{database_id}/collections/{collection_id}/attributes/{key}:migrate"},
	{RPC: "CreateIndex", HTTP: "POST /v1/server/databases/{database_id}/collections/{collection_id}/indexes"},
	{RPC: "DeleteIndex", HTTP: "DELETE /v1/server/databases/{database_id}/collections/{collection_id}/indexes/{index_id}"},
	{RPC: "CreateDocument", HTTP: "POST /v1/server/databases/{database_id}/collections/{collection_id}/documents"},
	{RPC: "ListDocuments", HTTP: "GET /v1/server/databases/{database_id}/collections/{collection_id}/documents; POST .../documents:list"},
	{RPC: "GetDocument", HTTP: "GET /v1/server/databases/{database_id}/collections/{collection_id}/documents/{document_id}"},
	{RPC: "UpdateDocument", HTTP: "PATCH /v1/server/databases/{database_id}/collections/{collection_id}/documents/{document_id}"},
	{RPC: "UpsertDocument", HTTP: "PUT /v1/server/databases/{database_id}/collections/{collection_id}/documents/{document_id}"},
	{RPC: "DeleteDocument", HTTP: "DELETE /v1/server/databases/{database_id}/collections/{collection_id}/documents/{document_id}"},
	{RPC: "CountDocuments", HTTP: "GET /v1/server/databases/{database_id}/collections/{collection_id}/documents:count; POST .../documents:count"},
	{RPC: "AggregateDocuments", HTTP: "POST /v1/server/databases/{database_id}/collections/{collection_id}/documents:aggregate"},
	{RPC: "BulkUpdateDocuments", HTTP: "PATCH /v1/server/databases/{database_id}/collections/{collection_id}/documents:bulkUpdate"},
	{RPC: "BulkDeleteDocuments", HTTP: "POST /v1/server/databases/{database_id}/collections/{collection_id}/documents:bulkDelete"},
	{RPC: "ExecuteTransactions", HTTP: "POST /v1/server/databases/{database_id}/documents:execute-tx"},
	{RPC: "ListChanges", HTTP: "GET /v1/server/databases/{database_id}/collections/{collection_id}/changes"},
}

// databasesServiceFullName 是目录 resources 段绑定的 gRPC 服务全名。
const databasesServiceFullName = "/torchwood.server.v1.DatabasesService/"

// buildWellKnownPayload 构造目录 JSON（进程内一次；错误码与 scope 两段构造期
// 直读 PolicySet——proto 策略声明的唯一派生）。
func buildWellKnownPayload(policies *auth.PolicySet) []byte {
	errorCodes := databases.ErrorCodeCatalog()
	codeEntries := make([]map[string]any, 0, len(errorCodes))
	for code := range errorCodes {
		codeEntries = append(codeEntries, map[string]any{
			"code":      code,
			"retryable": errorCodes[code],
		})
	}

	verbs := make([]wellKnownVerb, 0, len(wellKnownVerbs))
	for _, v := range wellKnownVerbs {
		if rule := policies.HasAPIKeyScope(databasesServiceFullName + v.RPC); rule != nil {
			v.Scope = string(rule.Resource) + "." + string(rule.Op)
		}
		verbs = append(verbs, v)
	}
	// scope 词表全量下发（M4：console 建 key 多选/SDK 常量的数据源）。
	vocab := auth.VocabularyFromPolicies(policies)
	scopeCatalog := make([]map[string]any, 0, len(vocab.Resources()))
	for _, res := range vocab.Resources() {
		scopeCatalog = append(scopeCatalog, map[string]any{
			"resource": string(res),
			"read":     vocab.HasOp(res, auth.ScopeRead),
			"write":    vocab.HasOp(res, auth.ScopeWrite),
		})
	}

	doc := map[string]any{
		"service": "torchwood",
		"$comment": "Machine-readable catalog for agents (redesign §4.1). " +
			"error_codes and scopes are generated from the single source of truth at server build time; " +
			"see docs/developer/14-agent-tools.md.",
		"query": map[string]any{
			"operators": wellKnownQueryOps,
			"notes": []string{
				"operators apply to declared attributes only; undeclared keys are rejected",
				"containsAny / containsAll require array=true attributes (server-side whitelist)",
				"negation is expressed by not* variants only; there is no generic NOT",
				"and / or nest recursively (depth <= 8)",
				"vector_search is a KNN operator outside the filter tree (typed AST only, no DSL string)",
			},
		},
		"error_codes":    codeEntries,
		"api_key_scopes": scopeCatalog,
		"resources": map[string]any{
			"databases": map[string]any{
				"service": "torchwood.server.v1.DatabasesService",
				"verbs":   verbs,
			},
		},
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		// 结构为静态字面量 + map 合并，marshal 不可失败；防御性兜底。
		return []byte(`{"service":"torchwood"}`)
	}
	return payload
}

// WellKnownHandler 提供 GET /.well-known/torchwood（Agent 可发现性目录，B10）。
type WellKnownHandler struct {
	payload []byte
}

// NewWellKnownHandler 构造目录 handler：payload 构造期生成一次（静态内容）。
func NewWellKnownHandler(policies *auth.PolicySet) *WellKnownHandler {
	return &WellKnownHandler{payload: buildWellKnownPayload(policies)}
}

// Register 把目录路由挂到 gateway mux（纯 HTTP 面，无 gRPC 对应物；公开端点
// ——目录只含契约元信息，不含项目数据）。
func (h *WellKnownHandler) Register(mux *runtime.ServeMux) {
	_ = mux.HandlePath("GET", "/.well-known/torchwood", h.serve)
}

func (h *WellKnownHandler) serve(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(h.payload)
}
