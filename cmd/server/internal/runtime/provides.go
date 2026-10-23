package runtime

import (
	"context"

	"github.com/google/wire"
	"github.com/lynx-go/grpcapi/policy"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
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
//	policy.Build + legacyProvider —— 读取 vendored authz.proto 旧注解，展开为
//	                                  库可求值的五档规范树（domainauth.TreeSpec）；
//	domainauth.AssertSemantic     —— 项目断言（规范树形、面前缀值域、档位分类、
//	                                  project_id 寻址不变量、PUBLIC 白名单、console 值域）。
//
// 产出的 *policy.Set 同时是声明面（View 派生文档/词表）与执行面（库 authz
// 拦截器）的唯一载体。等价性由 testdata/policies.golden.json（阶段 0 基线）
// 与 testdata/policies.manifest.golden.json（v3 树快照）锁定。
func buildMethodPolicies() (*policy.Set, error) {
	reg, err := domainauth.PolicyRegistry()
	if err != nil {
		return nil, err
	}
	set, err := policy.Build(context.Background(), policy.FromFiles(authzFileDescriptors()...), policy.Options{
		Provider:         legacyProvider{},
		Registry:         reg,
		Vocabulary:       domainauth.PolicyVocabulary(),
		DisableStreaming: true,
	})
	if err != nil {
		return nil, err
	}
	if err := domainauth.AssertSemantic(set); err != nil {
		return nil, err
	}
	return set, nil
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
