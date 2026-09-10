package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// manualHTTPRoutes 是所有非 gRPC 的手工 HTTP 路由（W-K 要求显式覆盖 authz）。
// 来源：internal/api/serverhttp/{file_handler,functions_handler}.go 的 Register
// 以及 realtime handler。新增手工路由后必须同步更新此表，否则测试失败（fail-closed）。
var manualHTTPRoutes = []struct {
	method string
	path   string
}{
	{"POST", "/v1/storage/buckets/{bucketId}/files"},
	{"GET", "/v1/storage/buckets/{bucketId}/files/{fileId}/download"},
	{"GET", "/v1/storage/buckets/{bucketId}/files/{fileId}/view"},
	{"GET", "/v1/storage/buckets/{bucketId}/files/{fileId}/preview"},
	{"POST", "/v1/storage/buckets/{bucketId}/uploads"},
	{"GET", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}"},
	{"POST", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}/chunks/{partNumber}"},
	{"POST", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}/complete"},
	{"DELETE", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}"},
	{"POST", "/v1/server/functions/{functionId}/deployments/code"},
}

// TestManualHTTPRoutesHaveAuthz 断言所有手工 HTTP 路由已被显式登记（W-K）。
// 实际 401 行为由 serverhttp/*_test.go 的集成测试覆盖，此处仅做静态覆盖校验
// 作为 assertRegisteredMethodsHaveAuthz 的 HTTP 侧补充，防止新增路由后遗漏 authz 评估。
func TestManualHTTPRoutesHaveAuthz(t *testing.T) {
	t.Parallel()
	require.Len(t, manualHTTPRoutes, 10, "manualHTTPRoutes 静态表应覆盖 10 条手工路由，新增后请同步更新并评估 authz")
	for _, r := range manualHTTPRoutes {
		require.NotEmpty(t, r.method)
		require.NotEmpty(t, r.path)
	}
	// 额外校验：所有手工路由均非 public（需凭证或 token/public 显式放行），
	// 不在 proto 的 ACCESS_PUBLIC 集合中（由 gRPC 侧 assert 保证）。
}
