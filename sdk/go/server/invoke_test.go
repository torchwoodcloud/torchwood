package server

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func TestInvokeJSONRoundTrip(t *testing.T) {
	lis, rec := newBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("k"))
	out, err := c.InvokeJSON(context.Background(), "/torchwood.server.v1.HealthService/Check", nil)
	require.NoError(t, err)
	// protojson Multiline 的 key: 后空格数由 detrand 随机（1 或 2 个），
	// 与 CLI 历史输出同一实现，格式一致；断言不依赖空格数。
	require.Regexp(t, `"status":\s+"ok"`, string(out))
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Equal(t, []string{"k"}, rec.md.Get("x-api-key"))
}

func TestInvokeJSONUnknownMethod(t *testing.T) {
	lis, _ := newBufconn(t)
	c := newTestClient(t, lis)
	_, err := c.InvokeJSON(context.Background(), "/torchwood.server.v1.Nope/Check", nil)
	require.ErrorContains(t, err, `torchwood: unknown method "/torchwood.server.v1.Nope/Check"`)
	// APIKeysService 被排除
	_, err = c.InvokeJSON(context.Background(), "/torchwood.server.v1.APIKeysService/ListAPIKeys", nil)
	require.ErrorContains(t, err, "unknown method")
	// 非 server v1 包被拒绝
	_, err = c.InvokeJSON(context.Background(), "/torchwood.client.v1.AccountService/SignIn", nil)
	require.ErrorContains(t, err, "unknown method")
}

func TestInvokeJSONBadJSON(t *testing.T) {
	lis, _ := newBufconn(t)
	c := newTestClient(t, lis)
	_, err := c.InvokeJSON(context.Background(), "/torchwood.server.v1.HealthService/Check", []byte(`{"nope": 1}`))
	require.Error(t, err) // 未知字段报错（DiscardUnknown=false）
}

// expectedServerMethodCount 是 torchwood.server.v1 包（排除 APIKeysService）
// 的方法总数精确快照。更新流程：proto 新增/删除 RPC 后运行本文件测试——
// TestInvokeJSONCompleteness 与 TestTypedWrappersCoverAllServerMethods 会
// 同时失败并报出实际 count；先为新增方法补齐 Client 类型化 wrapper（见
// client.go 的服务字段与各 service_*.go），再把本常量改为测试输出的实际值，
// 并在此注明变更来源（日期 + proto 文件）。禁止只改数字不加 wrapper。
//
// 快照历史：v0.1.0 时点 = 112（2026-08-24，Round4 J3-4 收紧，此前为 >60 下限）；
// 113 → 114（2026-09-04，databases.proto 新增 AggregateDocuments）；
// 114 → 115（2026-09-04，databases.proto 新增 ListChanges——阶段④事件补偿）；
// 115 → 116（2026-09-05，databases.proto 新增 ExportCollectionSchema——B10
// 集合契约 JSON Schema 导出，wrapper：DatabasesService.ExportCollectionSchema）；
// 116 → 119（2026-09-05，databases.proto 新增 Restore/Retire/MigrateAttribute
// ——B4 schema 演进生命周期 §4.6，wrapper 同名三方法）；
// 119 → 123（2026-09-08，本次 generate:proto 把此前未传播的生成物刷新到位：
// projects.proto 邀请码 Create/List/DeleteInviteCode（T-03）补 wrapper，
// 新增 UpdateOAuthRedirectAllowlist——OAuth 重定向白名单，wrapper 同名）；
// 123 → 124（2026-09-09，functions.proto 新增 SetFunctionScopes——P0 执行
// 身份 declared_scopes 全量替换，wrapper：FunctionsService.SetFunctionScopes）；
// 124 → 128（2026-09-09，functions.proto 新增触发器管理四方法——P1 触发器
// 模块，wrapper：FunctionsService.CreateFunctionTrigger / ListFunctionTriggers /
// DeleteFunctionTrigger / RotateFunctionTriggerToken）；
// 128 → 129（2026-09-11，audit_logs.proto 新增审计日志查询——wrapper：
// AuditLogsService.ListAuditLogs）；
// 129 → 132（2026-09-12，leaderboards.proto 新增排行榜 server 面——wrapper：
// LeaderboardsService.SubmitLeaderboardScore / GetLeaderboardEntry /
// ListLeaderboardTop）。
const expectedServerMethodCount = 132

// TestInvokeJSONCompleteness 遍历 protoregistry.GlobalFiles 中 torchwood.server.v1
// 包的全部方法（排除 APIKeysService），断言每个方法都能被解析并用空 JSON 构造
// 请求——防包名白名单/新增 proto 文件回归；同时用精确快照锁死方法总数，
// 防止静默增删 RPC。
func TestInvokeJSONCompleteness(t *testing.T) {
	count := 0
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Package() != "torchwood.server.v1" {
			return true
		}
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			ms := svc.Methods()
			for j := 0; j < ms.Len(); j++ {
				m := ms.Get(j)
				// gRPC 路径：/pkg.Service/Method（svc.FullName() 为点分隔全名）
				method := "/" + string(svc.FullName()) + "/" + string(m.Name())
				_, err := findServerMethod(method)
				if svc.Name() == "APIKeysService" {
					require.Error(t, err)
					continue
				}
				require.NoError(t, err, "method %s", m.FullName())
				count++
			}
		}
		return true
	})
	require.Equal(t, expectedServerMethodCount, count,
		"server 方法数与快照不符：若为新增 RPC 请同步补齐 Client wrapper 并更新 expectedServerMethodCount（见该常量注释）")
}

// TestTypedWrappersCoverAllServerMethods 反射断言：registry 中每个 server
// 方法（排除 APIKeysService）在 Client 对应 Service wrapper 上都存在同名
// 导出方法。wrapper 字段名 = proto 服务名去掉 Service 后缀（Health / Users /
// Databases ...），方法名与 proto RPC 名一致——拦住类型化封装面静默滞后于
// proto 演进（audit R4 §G-P2）。
func TestTypedWrappersCoverAllServerMethods(t *testing.T) {
	clientType := reflect.TypeOf(&Client{})
	count := 0
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Package() != "torchwood.server.v1" {
			return true
		}
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			if svc.Name() == "APIKeysService" {
				continue // 服务端禁调，SDK 有意不提供（见 findServerMethod）
			}
			wrapper, ok := clientType.Elem().FieldByName(strings.TrimSuffix(string(svc.Name()), "Service"))
			require.True(t, ok, "Client 缺少 %s 的 wrapper 字段", svc.FullName())
			require.Equal(t, reflect.Ptr, wrapper.Type.Kind(), "%s wrapper 应为指针类型", svc.FullName())

			ms := svc.Methods()
			for j := 0; j < ms.Len(); j++ {
				name := string(ms.Get(j).Name())
				// wrapper.Type 为 *Service（指针类型方法集含指针接收者方法）。
				m, ok := wrapper.Type.MethodByName(name)
				require.True(t, ok, "%s.%s 缺少同名词导出方法（新增 RPC 未补 wrapper？）",
					svc.FullName(), name)
				require.Empty(t, m.PkgPath, "%s.%s 必须是导出方法", svc.FullName(), name)
				count++
			}
		}
		return true
	})
	require.Equal(t, expectedServerMethodCount, count,
		"wrapper 覆盖数应与方法快照一致")
}
