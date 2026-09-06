package serverhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwooddev/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	"github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// wellKnownTestPolicies 按 wellKnownVerbs 清单构造最小 databases 策略
// （真实策略由 runtime.PolicySet 注入；本包测试不得 import runtime——会循环）。
func wellKnownTestPolicies() *auth.PolicySet {
	pols := make([]auth.MethodPolicy, 0, len(wellKnownVerbs))
	for _, v := range wellKnownVerbs {
		op := auth.ScopeWrite
		for _, prefix := range []string{"List", "Get", "Count", "Export"} {
			if strings.HasPrefix(v.RPC, prefix) {
				op = auth.ScopeRead
				break
			}
		}
		pols = append(pols, auth.MethodPolicy{
			Method: databasesServiceFullName + v.RPC, Service: databasesServiceFullName,
			Access: auth.AccessServer, Scope: &auth.ScopeRule{Resource: auth.ScopeDatabases, Op: op},
		})
	}
	set, err := auth.NewPolicySet(pols)
	if err != nil {
		panic(err)
	}
	return set
}

// serveWellKnown 经真实 runtime.ServeMux 注册路径后发起 GET（端点行为级测试）。
func serveWellKnown(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	mux := runtime.NewServeMux()
	NewWellKnownHandler(wellKnownTestPolicies()).Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/torchwood", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestWellKnownEndpoint（B10 判据：端点 + 内容结构断言）：GET /.well-known/
// torchwood 返回 200 + application/json，且 query/error_codes/resources 三段
// 齐备。
func TestWellKnownEndpoint(t *testing.T) {
	rec := serveWellKnown(t)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	require.Equal(t, "torchwood", doc["service"])

	query := doc["query"].(map[string]any)
	operators := query["operators"].([]any)
	require.NotEmpty(t, operators)

	errorCodes := doc["error_codes"].([]any)
	require.NotEmpty(t, errorCodes)

	scopes := doc["api_key_scopes"].([]any)
	require.NotEmpty(t, scopes, "scope 词表下发段应非空（M4）")

	resources := doc["resources"].(map[string]any)["databases"].(map[string]any)
	verbs := resources["verbs"].([]any)
	require.NotEmpty(t, verbs)
}

// TestWellKnownErrorCodesSync（B10 判据：与域码表同步的防漂移断言）：目录
// error_codes 与 databases.ErrorCodeCatalog()（errors.go 域码常量清单 +
// retryable 静态表）完全一致——errors.go 新增域码未进 allDomainCodes、或
// ErrorCodeCatalog 消费方漂移时此测试即红。
func TestWellKnownErrorCodesSync(t *testing.T) {
	body := serveWellKnown(t).Body.Bytes()
	var doc struct {
		ErrorCodes []struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error_codes"`
	}
	require.NoError(t, json.Unmarshal(body, &doc))

	got := make(map[string]bool, len(doc.ErrorCodes))
	for _, e := range doc.ErrorCodes {
		got[e.Code] = e.Retryable
	}
	want := databases.ErrorCodeCatalog()
	require.Equal(t, want, got)

	// 抽样语义复核：OCC 冲突可重试、权限拒绝不可重试。
	require.True(t, got[databases.ErrCodeVersionConflict])
	require.False(t, got[databases.ErrCodePermissionDenied])
}

// TestWellKnownOperatorsSync：目录 operators 与 shared.v1.Filter oneof（expr）
// 的字段全集一致——proto 新增算子未登记目录时此测试即红（防漂移）。
func TestWellKnownOperatorsSync(t *testing.T) {
	body := serveWellKnown(t).Body.Bytes()
	var doc struct {
		Query struct {
			Operators []wellKnownQueryOperator `json:"operators"`
		} `json:"query"`
	}
	require.NoError(t, json.Unmarshal(body, &doc))

	byProtoField := make(map[string]wellKnownQueryOperator, len(doc.Query.Operators))
	for _, op := range doc.Query.Operators {
		require.NotContains(t, byProtoField, op.ProtoField, "算子重复登记: %s", op.ProtoField)
		byProtoField[op.ProtoField] = op
	}

	oneofFields := sharedv1.File_shared_v1_query_proto.Messages().
		ByName("Filter").Oneofs().ByName("expr").Fields()
	for i := 0; i < oneofFields.Len(); i++ {
		field := string(oneofFields.Get(i).Name())
		op, ok := byProtoField[field]
		require.True(t, ok, "proto Filter oneof 字段 %q 未登记目录", field)
		if field == "and" || field == "or" {
			require.Equal(t, "combinator", op.Kind, field)
		} else {
			require.Equal(t, "comparison", op.Kind, field)
		}
	}
	require.Len(t, doc.Query.Operators, oneofFields.Len())

	// 数组算子标注（§10.5 P0）：containsAny/containsAll 仅 array=true 属性。
	require.True(t, byProtoField["contains_any"].ArrayOnly)
	require.True(t, byProtoField["contains_all"].ArrayOnly)
	require.False(t, byProtoField["eq"].ArrayOnly)
}

// TestWellKnownResourcesSync：resources.databases.verbs 与 proto
// DatabasesService 方法全集一致，且 scope 与 auth.APIKeyScopeRules()（单一
// 事实源）逐一匹配——服务面新增/删除 RPC 未同步目录、或 scope 规则漂移时
// 此测试即红。
func TestWellKnownResourcesSync(t *testing.T) {
	body := serveWellKnown(t).Body.Bytes()
	var doc struct {
		Resources struct {
			Databases struct {
				Service string `json:"service"`
				Verbs   []struct {
					RPC   string `json:"rpc"`
					HTTP  string `json:"http"`
					Scope string `json:"scope"`
				} `json:"verbs"`
			} `json:"databases"`
		} `json:"resources"`
	}
	require.NoError(t, json.Unmarshal(body, &doc))
	require.Equal(t, "torchwood.server.v1.DatabasesService", doc.Resources.Databases.Service)

	serviceDesc := serverv1.File_server_v1_databases_proto.Services().ByName("DatabasesService")
	protoMethods := map[string]struct{}{}
	methods := serviceDesc.Methods()
	for i := 0; i < methods.Len(); i++ {
		protoMethods[string(methods.Get(i).Name())] = struct{}{}
	}

	verbRPCs := map[string]struct{}{}
	for _, v := range doc.Resources.Databases.Verbs {
		require.NotContains(t, verbRPCs, v.RPC, "动词重复登记: %s", v.RPC)
		verbRPCs[v.RPC] = struct{}{}
		require.Contains(t, v.HTTP, " ", "动词 %s 缺 HTTP 形态", v.RPC)
		// scope 与 proto 注解（单一事实源）逐动词锁定——注解漂移即红。
		md := serviceDesc.Methods().ByName(protoreflect.Name(v.RPC))
		require.NotNil(t, md, "动词 %s 不在 proto 服务里", v.RPC)
		ma, _ := proto.GetExtension(md.Options(), sharedv1.E_MethodAuth).(*sharedv1.MethodAuth)
		require.NotNil(t, ma, "动词 %s 缺 method_auth 注解", v.RPC)
		sc := ma.GetApiKeyScope()
		require.NotNil(t, sc, "动词 %s 缺 api_key_scope", v.RPC)
		want := strings.ToLower(strings.TrimPrefix(sc.GetResource().String(), "SCOPE_RESOURCE_")) +
			"." + strings.ToLower(strings.TrimPrefix(sc.GetOp().String(), "SCOPE_OP_"))
		require.Equal(t, want, v.Scope, "动词 %s scope 与 proto 注解不一致", v.RPC)
	}
	require.Equal(t, protoMethods, verbRPCs, "目录动词清单必须与 proto 方法全集一致")

	// 方法描述符遍历形态自检（protoreflect 正常返回 ≥26 个方法）。
	require.GreaterOrEqual(t, len(protoMethods), 26)
}
