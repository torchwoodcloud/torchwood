package auth

import (
	"context"
	"fmt"
	"sort"

	"github.com/lynx-go/grpcapi/policy"
)

// 本文件是 v3 迁移期的 domain 策略类型：MethodPolicy 保持 torchwood 既有
// 断言/矩阵/文档消费的字段形状，从 grpcapi v3 policy.Set 经规范树识别
// （View，见 policy_tree.go）派生。params-JSON 编解码（legacy 双投影）已
// 退役——树是唯一策略载体，执行走库 RequirementDecider。

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

// MethodPolicy 是规范树的单方法视图（断言/矩阵/文档/golden 消费；派生投影，
// 不是第二棵策略树，也不参与执行）。
type MethodPolicy struct {
	Method, Service string
	Access          AccessLevel
	Permissions     []string
	AdminRoles      []string
	Scope           *ScopeRule
	RequestFields   []string
	IsStreaming     bool
}

// PolicySet 包装 v3 Set 并缓存 View 派生的方法视图（迁移期形状；执行点已
// 不消费本类型——gRPC 链走库 authz 拦截器）。
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

// FromSet 把 v3 policy.Set 逐方法识别为 domain 视图。非规范树形（legacy
// provider 只产出五档规范形）返回错误——fail-closed。
func FromSet(set *policy.Set) (*PolicySet, error) {
	if set == nil {
		return nil, fmt.Errorf("policy.Set is nil")
	}
	out := &PolicySet{raw: set, methods: make(map[string]MethodPolicy, set.Len())}
	for _, op := range set.Operations() {
		mp, err := View(op)
		if err != nil {
			return nil, fmt.Errorf("method %s: %w", op.ID, err)
		}
		if _, dup := out.methods[mp.Method]; dup {
			return nil, fmt.Errorf("duplicate method policy %s", mp.Method)
		}
		out.methods[mp.Method] = mp
	}
	return out, nil
}

// Raw 返回 v3 Set（启动断言与库执行点消费）。手工构造的测试集没有 raw，
// 返回永远拒绝的存在性投影——误接运行时授权时 fail-closed。
func (s *PolicySet) Raw() *policy.Set {
	if s == nil {
		return nil
	}
	if s.raw != nil {
		return s.raw
	}
	denyKind := policy.RequirementKind{
		Name:         "legacy_projection_deny",
		DefaultMatch: policy.MatchAny,
		Evaluate: func(_ context.Context, _ policy.EvalContext, _ policy.Requirement) (bool, error) {
			return false, nil
		},
	}
	reg, err := policy.NewRegistry(denyKind)
	if err != nil {
		return nil
	}
	ops := make([]policy.Operation, 0, len(s.methods))
	for _, p := range s.methods {
		ops = append(ops, policy.Operation{
			ID:       p.Method,
			Kind:     policy.KindGRPC,
			Service:  p.Service,
			Exposure: policy.ExposureInternal,
			Policy:   policy.Policy{Require: []policy.Requirement{{Kind: denyKind.Name}}},
		})
	}
	set, err := policy.NewSetWithRegistry(reg, ops...)
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
