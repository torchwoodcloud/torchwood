package runtime

import (
	"fmt"
	"strings"

	"github.com/lynx-go/grpcapi/policy"
	"github.com/lynx-go/grpcapi/policy/scope"
	legacypb "github.com/torchwoodcloud/torchwood/genproto/grpcapi/v1"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// legacyProvider 读取 vendored authz.proto 的旧注解，展开为库可求值的五档
// 规范树（domainauth.TreeSpec.Expand，见 domain/auth/policy_tree.go）。这是
// 路径 A 的声明桥：树由 grpcapi authz.RequirementDecider 直接执行，不再经
// params-JSON 编码/解码绕行（legacy 双投影已退役）。
type legacyProvider struct{}

func (legacyProvider) File(fd protoreflect.FileDescriptor) (policy.FileSpec, bool, error) {
	return policy.FileSpec{API: apiOfPackage(string(fd.Package())), SchemaVersion: 1}, true, nil
}

func (legacyProvider) Service(sd protoreflect.ServiceDescriptor) (policy.ServiceSpec, bool, error) {
	opts := sd.Options()
	if opts == nil {
		return policy.ServiceSpec{}, false, nil
	}
	ext := proto.GetExtension(opts, legacypb.E_ServiceAuth)
	sa, _ := ext.(*legacypb.ServiceAuth)
	if sa == nil {
		return policy.ServiceSpec{}, false, nil
	}
	p, err := domainauth.TreeSpec{Access: accessFromLegacy(sa.GetDefaultAccess())}.Expand()
	if err != nil {
		// 服务级默认无法展开为规范树（如 SERVER 默认未带 api_key_scope——
		// 合法声明形态，方法全部自带 method_auth 时默认从不被继承）：
		// 不注册默认策略；真正依赖默认的方法会在 Build 期报 missing
		// policy，fail-closed 与旧 AssertSemantic 路径等价。
		return policy.ServiceSpec{Label: legacyAccessName(sa.GetDefaultAccess())}, true, nil
	}
	return policy.ServiceSpec{DefaultPolicy: &p, Label: legacyAccessName(sa.GetDefaultAccess())}, true, nil
}

func (legacyProvider) Method(md protoreflect.MethodDescriptor) (policy.MethodSpec, bool, error) {
	opts := md.Options()
	if opts == nil {
		return policy.MethodSpec{}, false, nil
	}
	exposure := policy.ExposureInternal
	if hasHTTP(opts) {
		exposure = policy.ExposurePublic
	}
	ext := proto.GetExtension(opts, legacypb.E_MethodAuth)
	ma, _ := ext.(*legacypb.MethodAuth)
	if ma == nil {
		return policy.MethodSpec{Exposure: exposure, StreamAuth: policy.StreamAuthOpen}, true, nil
	}
	access := ma.GetAccess()
	if access == legacypb.AccessLevel_ACCESS_LEVEL_UNSPECIFIED {
		if sd, ok := md.Parent().(protoreflect.ServiceDescriptor); ok {
			access = serviceDefaultAccess(sd)
		}
	}
	spec := domainauth.TreeSpec{
		Access:            accessFromLegacy(access),
		Permissions:       ma.GetPermissions(),
		AdminRoles:        ma.GetAdminRoles(),
		InstanceAddressed: instanceAddressed(ma.GetApiKeyScope(), md.Input()),
	}
	if rule := ma.GetApiKeyScope(); rule != nil {
		spec.Scope = &domainauth.ScopeRule{Resource: rule.GetResource(), Op: domainauth.ScopeOp(scopeOpName(rule.GetOp()))}
	}
	p, err := spec.Expand()
	if err != nil {
		return policy.MethodSpec{}, false, fmt.Errorf("method %s: %w", md.FullName(), err)
	}
	return policy.MethodSpec{
		Policy:     &p,
		Exposure:   exposure,
		Label:      legacyAccessName(access),
		StreamAuth: policy.StreamAuthOpen,
	}, true, nil
}

// instanceAddressed 报告请求消息是否携带 scope 资源的实例寻址字段
// （resource 门只对可寻址且有寻址字段的方法展开；全集型方法无实例语义）。
func instanceAddressed(rule *legacypb.APIKeyScope, input protoreflect.MessageDescriptor) bool {
	if rule == nil || input == nil {
		return false
	}
	for _, name := range domainauth.InstanceFields(rule.GetResource()) {
		if input.Fields().ByName(protoreflect.Name(name)) != nil {
			return true
		}
	}
	return false
}

func legacyAccessName(access legacypb.AccessLevel) string {
	switch access {
	case legacypb.AccessLevel_ACCESS_LEVEL_PUBLIC:
		return "PUBLIC"
	case legacypb.AccessLevel_ACCESS_LEVEL_END_USER:
		return "END_USER"
	case legacypb.AccessLevel_ACCESS_LEVEL_SERVER:
		return "SERVER"
	case legacypb.AccessLevel_ACCESS_LEVEL_PERMISSION:
		return "PERMISSION"
	case legacypb.AccessLevel_ACCESS_LEVEL_SYSTEM:
		return "SYSTEM"
	default:
		return "UNSPECIFIED"
	}
}

func serviceDefaultAccess(sd protoreflect.ServiceDescriptor) legacypb.AccessLevel {
	opts := sd.Options()
	if opts == nil {
		return legacypb.AccessLevel_ACCESS_LEVEL_UNSPECIFIED
	}
	ext := proto.GetExtension(opts, legacypb.E_ServiceAuth)
	sa, _ := ext.(*legacypb.ServiceAuth)
	if sa == nil {
		return legacypb.AccessLevel_ACCESS_LEVEL_UNSPECIFIED
	}
	return sa.GetDefaultAccess()
}

func scopeOpName(op legacypb.ScopeOp) string {
	switch op {
	case legacypb.ScopeOp_SCOPE_OP_READ:
		return "read"
	case legacypb.ScopeOp_SCOPE_OP_WRITE:
		return "write"
	case legacypb.ScopeOp_SCOPE_OP_ADMIN:
		return "admin"
	default:
		return ""
	}
}

// accessFromLegacy 把 proto 枚举映射为 domain 档位（词表主权在 domain）。
func accessFromLegacy(access legacypb.AccessLevel) domainauth.AccessLevel {
	switch access {
	case legacypb.AccessLevel_ACCESS_LEVEL_PUBLIC:
		return domainauth.AccessPublic
	case legacypb.AccessLevel_ACCESS_LEVEL_END_USER:
		return domainauth.AccessEndUser
	case legacypb.AccessLevel_ACCESS_LEVEL_SERVER:
		return domainauth.AccessServer
	case legacypb.AccessLevel_ACCESS_LEVEL_PERMISSION:
		return domainauth.AccessPermission
	case legacypb.AccessLevel_ACCESS_LEVEL_SYSTEM:
		return domainauth.AccessSystem
	default:
		return domainauth.AccessLevelUnspecified
	}
}

func apiOfPackage(pkg string) string {
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

func hasHTTP(opts proto.Message) bool {
	ext := proto.GetExtension(opts, annotations.E_Http)
	rule, _ := ext.(*annotations.HttpRule)
	return rule != nil
}

// noInstanceTarget 是「本请求不寻址任何实例」的哨兵：合法资源实例 ID 的
// 字符集（数据库 ^[a-z][a-z0-9]{0,27}$ / 桶 ^[0-9a-zA-Z_-]{1,64}$）都不含
// 控制字符，哨兵永不等于任何实例 token 的目标段。旧语义：无目标的方法对
// 实例限定 scope 恒不匹配（403），其余 scope 形态不受影响——提取器返回
// 哨兵而非错误，避免把空 id 请求放大成 Internal。
const noInstanceTarget = "\x00no-instance-target"

// instanceTargetID 是注册进库 resource kind 的实例 ID 提取器：
//   - proto.Message：按 database_id → bucket_id → id 字段序取第一个非空值
//     （与旧 apiKeyScopeTargets 的 getter 优先级一致）；
//   - domainauth.TargetedRequest：serverhttp 面（存储 multipart 等无 proto
//     请求体）由传输层显式携带目标实例；
//   - 其余形态（nil 流式 open-time、未知类型）不寻址实例，返回哨兵。
func instanceTargetID(req any) (string, error) {
	switch v := req.(type) {
	case domainauth.TargetedRequest:
		if v.ID == "" {
			return noInstanceTarget, nil
		}
		return v.ID, nil
	case protoreflect.ProtoMessage:
		m := v.ProtoReflect()
		for _, name := range domainauth.InstanceExtractorFields {
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

// legacyRegistry 是 torchwood 的 kind 注册表：全部 kind 来自库（claim/
// principal/scope/resource），torchwood 不再注册自定义执行 kind。
func legacyRegistry() (*policy.Registry, error) {
	resourceKind, err := scope.ResourceKind(scope.ResourceOptions{
		ID:                instanceTargetID,
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
