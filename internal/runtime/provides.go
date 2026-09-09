package runtime

import (
	"github.com/google/wire"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// ProviderSet 收纳组装根的运行时服务（Round4 J4-4）：
// 原寄生于 internal/infra 的三项 gRPC/HTTP 运行时已迁至 internal/runtime，
// 保持对 infra 的单向依赖（runtime → infra/auth 等适配器，infra 不再反向依赖 api）。
var ProviderSet = wire.NewSet(
	NewGRPCServer,
	NewGRPCGatewayServer,
	NewMetricsServer,
	NewConsoleHandler,
	ProvideMethodPolicies,
	ProvideScopeVocabulary,
)

// ProvideMethodPolicies 收集 proto 策略注解为 PolicySet（全进程唯一收集点，
// 含 AssertSemantic 语义断言——违例启动失败）。策略唯一声明在 proto。
func ProvideMethodPolicies() (*domainauth.PolicySet, error) {
	set, err := BuildMethodPolicies(authzFileDescriptors()...)
	if err != nil {
		return nil, err
	}
	if err := domainauth.AssertSemantic(set); err != nil {
		return nil, err
	}
	return set, nil
}

// ProvideScopeVocabulary 从策略注册表派生合法 scope 词表（key 创建校验、
// well-known 下发与 console/SDK 生成的单一来源）。
func ProvideScopeVocabulary(set *domainauth.PolicySet) *domainauth.ScopeVocabulary {
	return domainauth.VocabularyFromPolicies(set)
}
