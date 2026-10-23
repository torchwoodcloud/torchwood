package runtime

import (
	"fmt"
	"strings"

	"github.com/lynx-go/grpcapi/policy"
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

// legacyRegistry / 词汇表 / 实例提取器已下沉 domainauth（PolicyRegistry /
// PolicyVocabulary / InstanceTargetID，见 internal/domain/auth/registry.go）——
// interceptor、testutil 与 runtime 共用同一装配。
