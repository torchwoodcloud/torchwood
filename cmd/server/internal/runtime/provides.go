package runtime

import (
	"github.com/google/wire"
	"github.com/lynx-go/grpcapi/authz"
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

// buildMethodPolicies 是策略唯一收集入口（阶段 1 起机制本体在
// github.com/lynx-go/grpcapi/authz；原 enum 映射层 authz_policy.go 退役）：
//
//	authz.Build            —— 库内置断言（词表必填、SERVER 必带 scope、
//	                         PERMISSION 必带 permissions、streaming 门、
//	                         scope 资源在词表内、死 scope）；
//	END_USER 归一          —— permissions 为空时置 ["users"]（原收集器语义的
//	                         后处理形态；库不提供变换钩子）；
//	domainauth.AssertSemantic —— 项目断言（面前缀值域、档位分类、project_id
//	                         寻址不变量、PUBLIC 白名单、console 值域）。
//
// 等价性由 testdata/policies.golden.json（阶段 0 基线）锁定。
func buildMethodPolicies() (*authz.PolicySet, error) {
	set, err := authz.Build(authzFileDescriptors(), authz.Options{
		Vocabulary: authz.Vocabulary{ScopeResources: domainauth.ScopeVocabularyResources()},
	})
	if err != nil {
		return nil, err
	}
	normalized := make([]authz.MethodPolicy, 0, len(set.Methods()))
	for _, p := range set.Methods() {
		if p.Access == authz.AccessEndUser && len(p.Permissions) == 0 {
			p.Permissions = []string{domainauth.RoleEndUserTag}
		}
		normalized = append(normalized, p)
	}
	set, err = authz.NewPolicySet(normalized...)
	if err != nil {
		return nil, err
	}
	if err := domainauth.AssertSemantic(set); err != nil {
		return nil, err
	}
	return set, nil
}

// ProvideMethodPolicies 收集 proto 策略注解为 PolicySet（全进程唯一收集点，
// 含语义断言——违例启动失败）。策略唯一声明在 proto。
func ProvideMethodPolicies() (*domainauth.PolicySet, error) {
	return buildMethodPolicies()
}

// ProvideScopeVocabulary 从策略注册表派生合法 scope 词表（key 创建校验、
// well-known 下发与 console/SDK 生成的单一来源）。
func ProvideScopeVocabulary(set *domainauth.PolicySet) *domainauth.ScopeVocabulary {
	return domainauth.VocabularyFromPolicies(set)
}
