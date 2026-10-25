package auth

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lynx-go/grpcapi/policy"
	"github.com/lynx-go/grpcapi/policy/scope"
	"github.com/lynx-go/grpcapi/principal"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 本文件是授权执行的装配胶水：kind 注册表、词表、实例目标提取器、
// Principal 投影与手工策略装配。全部执行 kind 来自 grpcapi 库——torchwood
// 不注册自定义执行 kind，授权执行 = 库 RequirementDecider（grpcapi v1.0
// §11.4 执行侧迁移；legacy 双投影与 interceptor 手工档位判定已退役）。

// 库 principal.Kind 投影值（五档树的 principal_kind 门消费）。
const (
	PrincipalKindUser      = principal.KindUser   // "user"    ← ActorKindEndUser
	PrincipalKindAdmin     = principal.KindAdmin  // "admin"   ← ActorKindAdmin
	PrincipalKindAPIKey    = principal.KindAPIKey // "api_key" ← ActorKindService
	PrincipalKindExecution = "execution"          //           ← ActorKindExecution
	PrincipalKindSystem    = "system"             //           ← ActorKindSystem
)

// PolicyRegistry 是 torchwood 的授权 kind 注册表：authenticated/claim/
// principal_kind/role/admin_role/permission/scope/resource。permission 的
// registry 默认 MatchAll 对齐库约定；torchwood 旧执行语义是任一命中
// （HasAnyRole），TreeSpec 展开的 permission requirement 恒显式 MatchAny。
func PolicyRegistry() (*policy.Registry, error) {
	resourceKind, err := scope.ResourceKind(scope.ResourceOptions{
		ID:                InstanceTargetID,
		VocabularyDomain:  "resource",
		RequireVocabulary: true,
	})
	if err != nil {
		return nil, err
	}
	return policy.NewRegistry(
		policy.KindAuthenticated(),
		policy.KindClaim(),
		policy.KindPrincipal(),
		policy.ClaimKind("role", "roles", policy.MatchAny),
		policy.ClaimKind("admin_role", "admin_roles", policy.MatchAny),
		policy.ClaimKind("permission", "permissions", policy.MatchAll),
		scope.Kind(scope.Options{
			Claim:             "scopes",
			VocabularyDomain:  "scope",
			RequireVocabulary: true,
		}),
		resourceKind,
	)
}

// PolicyVocabulary 注册 scope/resource 两个词表域（同一资源全集：scope 门
// 与 resource 门各自按域校验声明值）。
func PolicyVocabulary() policy.Vocabulary {
	return policy.NewVocabulary(map[string][]string{
		"scope":    ScopeVocabularyResources(),
		"resource": ScopeVocabularyResources(),
	})
}

// noInstanceTarget 是「本请求不寻址任何实例」的哨兵：合法资源实例 ID 的
// 字符集（数据库 ^[a-z][a-z0-9]{0,27}$ / 桶 ^[0-9a-zA-Z_-]{1,64}$）都不含
// 控制字符，哨兵永不等于任何实例 token 的目标段。旧语义：无目标的方法对
// 实例限定 scope 恒不匹配（403），其余 scope 形态不受影响——提取器返回
// 哨兵而非错误，避免把空 id 请求放大成 Internal。
const noInstanceTarget = "\x00no-instance-target"

// InstanceTargetID 是注册进库 resource kind 的实例 ID 提取器：
//   - proto.Message：按 database_id → bucket_id → id 字段序取第一个非空值
//     （与旧 apiKeyScopeTargets 的 getter 优先级一致）；
//   - TargetedRequest：serverhttp 面（存储 multipart 等无 proto 请求体）
//     由传输层显式携带目标实例；
//   - 其余形态（nil 流式 open-time、未知类型）不寻址实例，返回哨兵。
func InstanceTargetID(req any) (string, error) {
	switch v := req.(type) {
	case TargetedRequest:
		if v.ID == "" {
			return noInstanceTarget, nil
		}
		return v.ID, nil
	case protoreflect.ProtoMessage:
		m := v.ProtoReflect()
		for _, name := range InstanceExtractorFields {
			fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
			if fd == nil || fd.Kind() != protoreflect.StringKind {
				continue
			}
			if s := m.Get(fd).String(); s != "" {
				return s, nil
			}
		}
		return noInstanceTarget, nil
	default:
		return noInstanceTarget, nil
	}
}

// ProjectPrincipal 把 torchwood shared.Principal 投影为库 principal.Principal
// （库 authz 拦截器的求值输入）。claim 投影约定（与五档树门一一对应）：
//   - Kind：end_user→user、admin→admin、service→api_key、execution→execution、
//     system→system；
//   - roles / permissions claim = Roles（PERMISSION 面角色门读 permissions，
//     等价旧 HasAnyRole(perms)；API key 的 Roles 是 keys 族串，天然不中
//     console/owner/admin 值域——维持「key 不得调 PERMISSION 面」）；
//   - admin_roles claim 仅 admin 主体携带（SERVER 面 admin 角色门；旧执行
//     仅对 ActorKindAdmin 检查该门）；
//   - scopes claim = Permissions（API key scope 串；执行身份已投影同款）。
func ProjectPrincipal(p *shared.Principal) *principal.Principal {
	if p == nil {
		return nil
	}
	lib := &principal.Principal{
		ID:   string(p.ActorID),
		Kind: ProjectKind(p.ActorKind),
		Raw:  p,
	}
	if len(p.Roles) > 0 {
		lib.Claims = map[string][]string{
			"roles":       append([]string(nil), p.Roles...),
			"permissions": append([]string(nil), p.Roles...),
		}
		if p.ActorKind == shared.ActorKindAdmin {
			lib.Claims["admin_roles"] = append([]string(nil), p.Roles...)
		}
	}
	if len(p.Permissions) > 0 {
		if lib.Claims == nil {
			lib.Claims = make(map[string][]string, 1)
		}
		lib.Claims["scopes"] = append([]string(nil), p.Permissions...)
	}
	return lib
}

// ProjectKind 映射 ActorKind → 库 principal kind。
func ProjectKind(k shared.ActorKind) string {
	switch k {
	case shared.ActorKindEndUser:
		return PrincipalKindUser
	case shared.ActorKindAdmin:
		return PrincipalKindAdmin
	case shared.ActorKindService:
		return PrincipalKindAPIKey
	case shared.ActorKindExecution:
		return PrincipalKindExecution
	case shared.ActorKindSystem:
		return PrincipalKindSystem
	default:
		return string(k)
	}
}

// NewSet 把 MethodPolicy 视图反向装配为可求值 *policy.Set（测试/手工装配
// 专用；生产装配 = legacyProvider + policy.Build）。请求字段含任一实例
// 寻址字段即展开 resource 门。
func NewSet(policies ...MethodPolicy) (*policy.Set, error) {
	reg, err := PolicyRegistry()
	if err != nil {
		return nil, err
	}
	ops := make([]policy.Operation, 0, len(policies))
	for _, p := range policies {
		spec := TreeSpec{
			Access:            p.Access,
			Permissions:       p.Permissions,
			AdminRoles:        p.AdminRoles,
			Scope:             p.Scope,
			InstanceAddressed: requestAddressesInstance(p.RequestFields),
		}
		tree, err := spec.Expand()
		if err != nil {
			return nil, fmt.Errorf("method %s: %w", p.Method, err)
		}
		ops = append(ops, policy.Operation{
			ID:            p.Method,
			Kind:          policy.KindGRPC,
			Service:       strings.TrimPrefix(p.Service, "/"),
			Policy:        tree,
			Exposure:      policy.ExposureInternal,
			Streaming:     p.IsStreaming,
			RequestFields: p.RequestFields,
		})
	}
	return policy.NewSetWithVocabulary(reg, PolicyVocabulary(), ops...)
}

func requestAddressesInstance(fields []string) bool {
	for _, f := range fields {
		if slices.Contains(InstanceExtractorFields, f) {
			return true
		}
	}
	return false
}
