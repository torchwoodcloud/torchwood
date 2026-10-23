package runtime

import (
	"context"
	"strings"

	"github.com/google/wire"
	"github.com/lynx-go/grpcapi/policy"
	"github.com/lynx-go/grpcapi/policy/protopolicy"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ProviderSet 收纳组装根的运行时服务（Round4 J4-4）：
// 原寄生于 internal/infra 的三项 gRPC/HTTP 运行时已迁至本包
// （cmd/server/internal/runtime，2026-09 起为 server 专属 internal 组件），
// 保持对 infra 的单向依赖（runtime → infra/auth 等适配器，infra 不再反向依赖 api）。
var ProviderSet = wire.NewSet(
	NewGRPCServer,
	NewGRPCGatewayServer,
	NewMetricsServer,
	NewDebugServer,
	NewConsoleHandler,
	ProvideMethodPolicies,
	ProvideScopeVocabulary,
)

// buildMethodPolicies 是策略唯一收集入口：
//
//	policy.Build + protopolicy   —— 读取业务 proto 的 policy.proto 声明
//	                                （路径 B codemod 后的规范树直 declaration）；
//	domainauth.AssertSemantic    —— 项目断言（规范树形、面前缀值域、档位分类、
//	                                project_id 寻址不变量、PUBLIC 白名单、console 值域）。
//
// 产出的 *policy.Set 同时是声明面（View 派生文档/词表）与执行面（库 authz
// 拦截器）的唯一载体。等价性由 testdata/policies.manifest.golden.json
// （v3 树快照，codemod 前后字节级不变）锁定。
func buildMethodPolicies() (*policy.Set, error) {
	reg, err := domainauth.PolicyRegistry()
	if err != nil {
		return nil, err
	}
	set, err := policy.Build(context.Background(), policy.FromFiles(authzFileDescriptors()...), policy.Options{
		Provider:    protopolicy.New(protopolicy.Options{}),
		Registry:    reg,
		Vocabulary:  domainauth.PolicyVocabulary(),
		APIResolver: apiOfPackage,
	})
	if err != nil {
		return nil, err
	}
	if err := domainauth.AssertSemantic(set); err != nil {
		return nil, err
	}
	return set, nil
}

// apiOfPackage 按 proto 包前缀推断 API 面（client/server/console）。
func apiOfPackage(fd protoreflect.FileDescriptor) string {
	pkg := string(fd.Package())
	switch {
	case strings.HasPrefix(pkg, "torchwood.client."):
		return "client"
	case strings.HasPrefix(pkg, "torchwood.server."):
		return "server"
	case strings.HasPrefix(pkg, "torchwood.console."):
		return "console"
	default:
		return "default"
	}
}

// ProvideMethodPolicies 收集 proto 策略注解为规范树策略集（全进程唯一收集
// 点，含语义断言——违例启动失败）。策略唯一声明在 proto；授权执行走库
// authz 拦截器（消费同一 *policy.Set）。
func ProvideMethodPolicies() (*policy.Set, error) {
	return buildMethodPolicies()
}

// ProvideScopeVocabulary 从策略集派生合法 scope 词表（key 创建校验、
// well-known 下发与 console/SDK 生成的单一来源）。
func ProvideScopeVocabulary(set *policy.Set) *domainauth.ScopeVocabulary {
	return domainauth.VocabularyFromPolicies(set)
}
