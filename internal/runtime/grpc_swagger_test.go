package runtime

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// businessFileDescriptors 复用 authzFileDescriptors 单一事实源
// （NewGRPCServer 的 collectMethodsByAccess 与本文件 swagger 覆盖检查同源）。
func businessFileDescriptors() []protoreflect.FileDescriptor {
	return authzFileDescriptors()
}

func accessLevelString(l sharedv1.AccessLevel) (string, bool) {
	switch l {
	case sharedv1.AccessLevel_ACCESS_PUBLIC:
		return "public", true
	case sharedv1.AccessLevel_ACCESS_END_USER:
		return "end_user", true
	case sharedv1.AccessLevel_ACCESS_SERVER:
		return "server", true
	case sharedv1.AccessLevel_ACCESS_PERMISSION:
		return "permission", true
	case sharedv1.AccessLevel_ACCESS_SYSTEM:
		return "system", true
	}
	return "", false
}

// domainAccessString 映射 domain AccessLevel → swagger x-torchwood-access 字符串。
func domainAccessString(l domainauth.AccessLevel) (string, bool) {
	switch l {
	case domainauth.AccessPublic:
		return "public", true
	case domainauth.AccessEndUser:
		return "end_user", true
	case domainauth.AccessServer:
		return "server", true
	case domainauth.AccessPermission:
		return "permission", true
	case domainauth.AccessSystem:
		return "system", true
	}
	return "", false
}

// swaggerOp 是 swagger.json 中单个 HTTP operation 的最小结构。
// 注意 default 响应位于 operation.responses.default（非 operation 顶层）。
type swaggerOp struct {
	OperationID string `json:"operationId"`
	XAccess     string `json:"x-torchwood-access"`
	Responses   struct {
		Default struct {
			Schema struct {
				Ref string `json:"$ref"`
			} `json:"schema"`
		} `json:"default"`
	} `json:"responses"`
}

// errorResponseRef 与各 service proto 文件级 openapiv2_swagger.responses.default
// 声明的引用经生成器解析后的定义名保持一致（legacy 命名：最短唯一后缀+1，
// Error/ErrorResponse/ErrorCode 消息名当前全局唯一，落在 v1 前缀下）。
const errorResponseRef = "#/definitions/v1ErrorResponse"

// TestSwaggerAccessExtensionMatchesCollectMethodsByAccess 断言每个
// genproto/**/*.swagger.json 中 operation 的有效 x-torchwood-access
// （operation 级扩展，未声明时继承 swagger 顶层值）与 collectMethodsByAccess
// 从 proto method_auth/service_auth 推导出的 access 完全一致（R10-P1-4）：
// 顶层扩展必须等于服务默认 access；operation 扩展与 method_auth 必须一致。
// 前提：仓库根执行过 task generate:proto（genproto 已入库，本测试直接可用）。
func TestSwaggerAccessExtensionMatchesCollectMethodsByAccess(t *testing.T) {
	descs := businessFileDescriptors()
	policies, err := BuildMethodPolicies(descs...)
	require.NoError(t, err)

	// 按收集器解析语义（方法级 method_auth 优先，否则服务级默认）推导每个
	// 方法的 access（直接消费 PolicySet，与启动路径同源）。
	accessOf := make(map[string]string)
	defaultOf := make(map[string]string) // service full name → 服务默认 access
	byProtoPath := make(map[string]protoreflect.FileDescriptor, len(descs))
	for _, fd := range descs {
		byProtoPath[fd.Path()] = fd
		for i := 0; i < fd.Services().Len(); i++ {
			s := fd.Services().Get(i)
			serviceName := string(s.FullName())
			def, ok := accessLevelString(resolveServiceDefaultAccess(s))
			require.True(t, ok, "service %s 无法解析默认 access", serviceName)
			defaultOf[serviceName] = def
		}
	}
	for _, p := range policies.Methods() {
		as, ok := domainAccessString(p.Access)
		require.True(t, ok, "method %s access 非法", p.Method)
		accessOf[p.Method] = as
	}

	genprotoDir := filepath.Join("..", "..", "genproto")
	seenMethods := make(map[string]int) // full method → 在 swagger 中出现的次数
	defaultRefOK := 0                   // default 响应正确引用 ErrorResponse 的 operation 数
	badDefaultRef := make([]string, 0, 4)
	checkedOps := 0
	checkedFiles := 0
	for _, sub := range []string{"server", "client", "console"} {
		dir := filepath.Join(genprotoDir, sub, "v1")
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".swagger.json") {
				continue
			}
			protoPath := sub + "/v1/" + strings.TrimSuffix(name, ".swagger.json") + ".proto"
			fd, ok := byProtoPath[protoPath]
			if !ok {
				// shared/ 纯消息文件与框架产物无业务服务，跳过。
				continue
			}
			services := fd.Services()
			require.Equal(t, 1, services.Len(), "%s 应恰好包含一个业务服务", protoPath)
			service := services.Get(0)
			serviceName := string(service.FullName())

			var doc struct {
				XAccess string                          `json:"x-torchwood-access"`
				Paths   map[string]map[string]swaggerOp `json:"paths"`
			}
			raw, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- 路径来自仓库内测试数据
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &doc))
			checkedFiles++

			wantDefault := defaultOf[serviceName]
			require.Equal(t, wantDefault, doc.XAccess,
				"%s 顶层 x-torchwood-access 应与服务默认 access 一致（%s）", name, serviceName)

			for _, ops := range doc.Paths {
				for _, op := range ops {
					if op.OperationID == "" {
						continue
					}
					// R4-J2-2：default 响应必须引用运行时真实错误体 ErrorResponse，
					// rpcStatus 属生成器默认值失真，回归即红。
					if op.Responses.Default.Schema.Ref == errorResponseRef {
						defaultRefOK++
					} else {
						badDefaultRef = append(badDefaultRef, name+"/"+op.OperationID+" → "+op.Responses.Default.Schema.Ref)
					}

					// operationId 格式为 "{Service}_{RPC}"；additional_bindings
					// 生成 "{Service}_{RPC}{N}" 后缀（如 HealthService_Check2）。
					sep := strings.LastIndex(op.OperationID, "_")
					require.Greater(t, sep, 0, "%s operationId %q 格式非法", name, op.OperationID)
					rpc := op.OperationID[sep+1:]
					method := findMethodByOperationID(service, rpc)
					require.NotNil(t, method, "%s operationId %q 找不到对应 RPC", name, op.OperationID)

					fullMethod := "/" + serviceName + "/" + string(method.Name())
					want, ok := accessOf[fullMethod]
					require.True(t, ok, "%s %q 不在 collectMethodsByAccess 结果中", name, fullMethod)

					effective := op.XAccess
					if effective == "" {
						effective = doc.XAccess
					}
					require.Equal(t, want, effective,
						"%s %s 有效 x-torchwood-access=%q 与 method_auth %q 不一致",
						name, op.OperationID, effective, want)
					seenMethods[fullMethod]++
					checkedOps++
				}
			}
		}
	}
	require.GreaterOrEqual(t, checkedFiles, 14, "genproto 业务 swagger 文件数量异常")
	require.GreaterOrEqual(t, checkedOps, 140, "swagger operation 数量异常（当前 %d）", checkedOps)
	// R4-J2-2：错误模型统一回归门禁。
	require.Empty(t, badDefaultRef,
		"以下 operation 的 default 响应未引用 %s（应重跑 task generate:proto）：\n%s",
		errorResponseRef, strings.Join(badDefaultRef, "\n"))
	require.GreaterOrEqual(t, defaultRefOK, checkedOps,
		"default 响应引用计数异常：ok=%d ops=%d", defaultRefOK, checkedOps)

	// R4-J2-5 反向覆盖率：collectMethodsByAccess 登记的每个方法都必须在
	// swagger 出现 ≥1 次。缺失通常意味着该 RPC 漏配 google.api.http 注解，
	// 会从 OpenAPI/机器可读面静默消失。
	missing := make([]string, 0, 4)
	for fm := range accessOf {
		if seenMethods[fm] == 0 {
			missing = append(missing, fm)
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"以下方法未出现在任何 swagger paths 中（漏配 http 注解？）：\n%s",
		strings.Join(missing, "\n"))
}

// TestSwaggerPropertyNamesAreSnakeCase 抽检 swagger definitions 的属性名
// 必须为 snake_case（R4-J2-1 要求 json_names_for_fields=false）。
func TestSwaggerPropertyNamesAreSnakeCase(t *testing.T) {
	genprotoDir := filepath.Join("..", "..", "genproto")
	snake := func(s string) bool {
		if s == "" {
			return true
		}
		for i, ch := range s {
			if ch >= 'A' && ch <= 'Z' {
				return false
			}
			if i == 0 && ch >= '0' && ch <= '9' {
				return false
			}
			if !(ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
				return false
			}
		}
		return true
	}
	var bad []string
	for _, sub := range []string{"server", "client", "console"} {
		dir := filepath.Join(genprotoDir, sub, "v1")
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".swagger.json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			require.NoError(t, err)
			var doc struct {
				Definitions map[string]struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"definitions"`
			}
			require.NoError(t, json.Unmarshal(raw, &doc))
			for defName, def := range doc.Definitions {
				for prop := range def.Properties {
					if prop == "@type" {
						continue
					}
					if !snake(prop) {
						bad = append(bad, e.Name()+"/"+defName+"."+prop)
					}
				}
			}
		}
	}
	require.Empty(t, bad, "以下 swagger 属性名非 snake_case（应重跑 task generate:proto 检查 json_names_for_fields）：\n%s", strings.Join(bad, "\n"))
}

// TestSwaggerNoRpcStatus 断言全部 genproto/**/*.swagger.json 不含 rpcStatus
// 定义或引用：生成器默认注入的 rpcStatus 与运行时错误体不符，buf.gen.yaml
// 已用 disable_default_errors=true 关闭注入（早期由 tools/openapifix 后处理
// 移除，工具删除后此断言接任守卫）。该名字再现通常意味着 buf 配置回退。
func TestSwaggerNoRpcStatus(t *testing.T) {
	genprotoDir := filepath.Join("..", "..", "genproto")
	files := 0
	var offenders []string
	err := filepath.WalkDir(genprotoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".swagger.json") {
			return nil
		}
		files++
		raw, err := os.ReadFile(path) // #nosec G304 -- 路径来自仓库内测试数据
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "rpcStatus") {
			offenders = append(offenders, filepath.ToSlash(path))
		}
		return nil
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, files, 28, "genproto swagger 文件数量异常（当前 %d）", files)
	require.Empty(t, offenders,
		"以下 swagger 仍含 rpcStatus（buf.gen.yaml 的 disable_default_errors 被回退？）：\n%s",
		strings.Join(offenders, "\n"))
}

// findMethodByOperationID 先按原名精确查找；未命中时按 additional_bindings
// 的 "{N}" 数字后缀解析（如 Check2），并校验 N 落在该方法真实声明过的
// HTTP 绑定索引范围内（R4-J2-5：防数字截断把未知 RPC 误配到形似方法）。
func findMethodByOperationID(service protoreflect.ServiceDescriptor, rpc string) protoreflect.MethodDescriptor {
	methods := service.Methods()
	for i := 0; i < methods.Len(); i++ {
		if string(methods.Get(i).Name()) == rpc {
			return methods.Get(i)
		}
	}
	// 仅当尾部是纯数字后缀时尝试绑定索引匹配：openapiv2 对
	// additional_bindings 从 2 起编号（主规则走精确匹配），因此要求
	// 2 <= N <= 该方法声明的绑定总数，基名必须真实存在（R4-J2-5：
	// 防数字截断把未知 RPC 误配到形似方法）。
	if idx := strings.LastIndexAny(rpc, "0123456789"); idx >= 0 && idx == len(rpc)-1 {
		base, numStr := rpc[:idx], rpc[idx:]
		if n, err := strconv.Atoi(numStr); err == nil && n >= 2 {
			for i := 0; i < methods.Len(); i++ {
				m := methods.Get(i)
				if string(m.Name()) == base && n <= methodHTTPBindingCount(m) {
					return m
				}
			}
		}
	}
	return nil
}

// methodHTTPBindingCount 返回方法声明的 google.api.http 绑定总数
// （主规则 1 + additional_bindings）。未声明 http 规则返回 0。
func methodHTTPBindingCount(m protoreflect.MethodDescriptor) int {
	if m.Options() == nil {
		return 0
	}
	rule, ok := proto.GetExtension(m.Options(), annotations.E_Http).(*annotations.HttpRule)
	if !ok || rule == nil {
		return 0
	}
	return 1 + len(rule.GetAdditionalBindings())
}
