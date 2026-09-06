package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/api/interceptor"
)

// TestAuthzCoverage_RealProtoRegistry（返工 R7）：以真实 proto registry
// （与 NewGRPCServer 的 collectMethodsByAccess 同源）复现启动期两条 fail-closed
// 断言——scope 表覆盖 ACCESS_API_KEY 方法集合、admin 角色表覆盖全部写方法。
// 此前该链条只在启动期执行，包测试全绿而 main 可处于启动 panic 状态
// （会话 #3 的 ExecuteTransactions 漏登即此缺口）；本测试使"新增写 RPC 漏登
// scope/角色表"直接变红。
func TestAuthzCoverage_RealProtoRegistry(t *testing.T) {
	t.Parallel()

	_, apiKeyMethods, _, err := collectMethodsByAccess(authzFileDescriptors()...)
	require.NoError(t, err)
	require.NotEmpty(t, apiKeyMethods, "真实 registry 必须能推导出 ACCESS_API_KEY 方法")

	require.NotPanics(t, func() {
		interceptor.AssertAPIKeyScopeCoverage(apiKeyMethods)
		interceptor.AssertAdminRoleWriteCoverage()
	}, "启动期 authz 覆盖断言在测试期必须同样成立（漏登 scope/角色表即红）")
}

// TestAuthzCoverage_DetectsFabricatedMethod：向方法集合注入虚构的写方法，
// 断言链条真的会抓漏（防上一测试自身失效——确认非空洞通过）。
// 角色表侧的注入检出（diff 纯函数的 missing/extra）由
// internal/api/interceptor/admin_roles_test.go 覆盖。
func TestAuthzCoverage_DetectsFabricatedMethod(t *testing.T) {
	t.Parallel()

	_, apiKeyMethods, _, err := collectMethodsByAccess(authzFileDescriptors()...)
	require.NoError(t, err)

	fabricated := append(append([]string{}, apiKeyMethods...),
		"/torchwood.server.v1.DatabasesService/FabricatedWriteThing")
	require.PanicsWithValue(t,
		"apiKeyScopeRules 与 ACCESS_API_KEY 方法集合不一致 (fail-closed): "+
			"proto 声明但规则表缺失=[/torchwood.server.v1.DatabasesService/FabricatedWriteThing]; 规则表多余=[]",
		func() { interceptor.AssertAPIKeyScopeCoverage(fabricated) },
	)
}
