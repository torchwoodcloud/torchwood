package runtime

import (
	"fmt"
	"strings"

	"github.com/lynx-go/grpcapi/policy"
	"github.com/lynx-go/grpcapi/policy/scope"
	legacypb "github.com/torchwoodcloud/torchwood/genproto/grpcapi/v1"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const legacyAccessKind = "legacy_access"

// legacyProvider 读取 vendored authz.proto 的旧注解，把旧字段编码进
// legacy_access requirement；domainauth.FromPolicySet 再转回 domain 形状。
// 这是迁移桥，不是 v3 最终策略模型。
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
	p := legacyPolicy(sa.GetDefaultAccess(), nil, nil, nil)
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
	p := legacyPolicy(access, ma.GetAdminRoles(), ma.GetApiKeyScope(), ma.GetPermissions())
	return policy.MethodSpec{
		Policy:     &p,
		Exposure:   exposure,
		Label:      legacyAccessName(access),
		StreamAuth: policy.StreamAuthOpen,
	}, true, nil
}

func legacyPolicy(access legacypb.AccessLevel, roles []string, scopeRule *legacypb.APIKeyScope, permissions []string) policy.Policy {
	params := map[string]string{"access": legacyAccessName(access)}
	if len(roles) > 0 {
		params["admin_roles"] = strings.Join(roles, ",")
	}
	if len(permissions) > 0 {
		params["permissions"] = strings.Join(permissions, ",")
	}
	if scopeRule != nil {
		params["scope_resource"] = scopeRule.GetResource()
		params["scope_op"] = scopeOpName(scopeRule.GetOp())
	}
	return policy.Policy{Require: []policy.Requirement{{Kind: legacyAccessKind, Params: params}}}
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

func legacyAccessKindDef() policy.RequirementKind {
	return policy.RequirementKind{
		Name:         legacyAccessKind,
		DefaultMatch: policy.MatchAny,
		Validate: func(req policy.Requirement, _ policy.Vocabulary) error {
			if req.Params["access"] == "" {
				return fmt.Errorf("legacy_access requires params.access")
			}
			return nil
		},
	}
}

func legacyRegistry() (*policy.Registry, error) {
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
		legacyAccessKindDef(),
	)
}
