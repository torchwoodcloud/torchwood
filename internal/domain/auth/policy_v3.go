package auth

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lynx-go/grpcapi/policy"
)

// 本文件是 v3 迁移期的 domain 策略类型：保持 torchwood 现有执行点/测试的
// 类型形状不变，同时从 grpcapi v3 policy.Set 转换。业务 proto 仍使用旧
// authz 注解，由 runtime.legacyProvider 读取后编码成 legacy_access 谓词。

type AccessLevel int32

const (
	AccessLevelUnspecified AccessLevel = 0
	AccessPublic           AccessLevel = 1
	AccessEndUser          AccessLevel = 2
	AccessServer           AccessLevel = 3
	AccessPermission       AccessLevel = 4
	AccessSystem           AccessLevel = 5
)

type ScopeOp string

const (
	ScopeRead  ScopeOp = "read"
	ScopeWrite ScopeOp = "write"
	ScopeAdmin ScopeOp = "admin"
)

type ScopeRule struct {
	Resource string
	Op       ScopeOp
}

// MethodPolicy 保持 v2 的字段形状，供既有拦截器/矩阵/断言消费。
type MethodPolicy struct {
	Method, Service string
	Access          AccessLevel
	Permissions     []string
	AdminRoles      []string
	Scope           *ScopeRule
	RequestFields   []string
	IsStreaming     bool
}

// PolicySet 是 domain 策略注册表；raw 保留 v3 Set 供库启动断言使用。
type PolicySet struct {
	raw     *policy.Set
	methods map[string]MethodPolicy
}

func NewPolicySet(policies ...MethodPolicy) (*PolicySet, error) {
	m := make(map[string]MethodPolicy, len(policies))
	for _, p := range policies {
		if _, dup := m[p.Method]; dup {
			return nil, fmt.Errorf("duplicate method policy %s", p.Method)
		}
		m[p.Method] = p
	}
	return &PolicySet{methods: m}, nil
}

// FromPolicySet 把 v3 policy.Set 转回 domain 形状。legacyProvider 用
// legacy_access requirement 编码旧字段；没有该 requirement 时按通用策略推断。
func FromPolicySet(set *policy.Set) (*PolicySet, error) {
	if set == nil {
		return nil, fmt.Errorf("policy.Set is nil")
	}
	out := &PolicySet{raw: set, methods: make(map[string]MethodPolicy, set.Len())}
	for _, op := range set.Operations() {
		mp := MethodPolicy{
			Method:        op.ID,
			Service:       "/" + strings.TrimPrefix(op.Service, "/"),
			RequestFields: append([]string(nil), op.RequestFields...),
			IsStreaming:   op.Streaming,
		}
		if req, ok := legacyAccessRequirement(op.Policy); ok {
			mp.Access = accessFromString(req.Params["access"])
			mp.Permissions = splitCSV(req.Params["permissions"])
			mp.AdminRoles = splitCSV(req.Params["admin_roles"])
			if res := req.Params["scope_resource"]; res != "" {
				mp.Scope = &ScopeRule{Resource: res, Op: ScopeOp(req.Params["scope_op"])}
			}
		} else {
			inferMethodPolicy(&mp, op.Policy)
		}
		if mp.Access == AccessEndUser && len(mp.Permissions) == 0 {
			mp.Permissions = []string{RoleEndUserTag}
		}
		if _, dup := out.methods[mp.Method]; dup {
			return nil, fmt.Errorf("duplicate method policy %s", mp.Method)
		}
		out.methods[mp.Method] = mp
	}
	return out, nil
}

func (s *PolicySet) Raw() *policy.Set {
	if s == nil {
		return nil
	}
	if s.raw != nil {
		return s.raw
	}
	// NewPolicySet（测试/手工构造）没有 v3 raw；构造一个仅用于
	// AssertAllRegisteredHavePolicy 的存在性投影。
	ops := make([]policy.Operation, 0, len(s.methods))
	for _, p := range s.methods {
		ops = append(ops, policy.Operation{
			ID:       p.Method,
			Kind:     policy.KindGRPC,
			Service:  p.Service,
			Exposure: policy.ExposureInternal,
			// 仅存在性投影：用 require authenticated 而非 anonymous，
			// 避免测试/手工 PolicySet 被误用于运行时授权时 fail-open。
			Policy: policy.Policy{Require: []policy.Requirement{{Kind: "authenticated"}}},
		})
	}
	set, err := policy.NewSet(ops...)
	if err != nil {
		return nil
	}
	return set
}

func (s *PolicySet) Get(method string) (MethodPolicy, bool) {
	if s == nil {
		return MethodPolicy{}, false
	}
	p, ok := s.methods[method]
	return p, ok
}

func (s *PolicySet) Methods() []MethodPolicy {
	if s == nil {
		return nil
	}
	out := make([]MethodPolicy, 0, len(s.methods))
	for _, p := range s.methods {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method < out[j].Method })
	return out
}

func (s *PolicySet) ScopeRule(method string) *ScopeRule {
	p, ok := s.Get(method)
	if !ok || p.Access != AccessServer || p.Scope == nil {
		return nil
	}
	rule := *p.Scope
	return &rule
}

func (s *PolicySet) AllowedAdminRoles(method string) []string {
	p, ok := s.Get(method)
	if !ok || p.Access != AccessServer {
		return nil
	}
	return append([]string(nil), p.AdminRoles...)
}

func legacyAccessRequirement(p policy.Policy) (policy.Requirement, bool) {
	for _, r := range p.Require {
		if r.Kind == "legacy_access" {
			return r, true
		}
	}
	return policy.Requirement{}, false
}

func accessFromString(s string) AccessLevel {
	switch strings.ToUpper(s) {
	case "PUBLIC":
		return AccessPublic
	case "END_USER":
		return AccessEndUser
	case "SERVER":
		return AccessServer
	case "PERMISSION":
		return AccessPermission
	case "SYSTEM":
		return AccessSystem
	default:
		return AccessLevelUnspecified
	}
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func inferMethodPolicy(mp *MethodPolicy, p policy.Policy) {
	if p.Anonymous {
		mp.Access = AccessPublic
		return
	}
	for _, r := range p.Require {
		switch r.Kind {
		case "principal_kind":
			for _, v := range r.Values {
				if v == "user" {
					mp.Access = AccessEndUser
					return
				}
			}
		case "permission":
			mp.Access = AccessPermission
			mp.Permissions = append(mp.Permissions, r.Values...)
			return
		case "scope":
			mp.Access = AccessServer
			if len(r.Values) > 0 {
				res, op := splitScopeValue(r.Values[0])
				mp.Scope = &ScopeRule{Resource: res, Op: ScopeOp(op)}
			}
			return
		case "admin_role", "role":
			mp.Access = AccessServer
			mp.AdminRoles = append(mp.AdminRoles, r.Values...)
		}
	}
}

func splitScopeValue(v string) (resource, op string) {
	if i := strings.LastIndex(v, ":"); i >= 0 {
		return v[:i], v[i+1:]
	}
	if i := strings.LastIndex(v, "."); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}
