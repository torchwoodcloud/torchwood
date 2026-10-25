package auth

import (
	"fmt"
	"sort"

	"github.com/lynx-go/grpcapi/policy"
)

// 本文件保留五档语义的类型与视图助手：MethodPolicy 是规范树（见
// policy_tree.go）的单方法视图，供断言/矩阵/文档/golden 消费——派生投影，
// 不是第二棵策略树，也不参与执行（授权执行 = 库 RequirementDecider）。
// 存储型 PolicySet 包装与 FromSet params-JSON 投影已退役（grpcapi v1.0
// §11.4 执行侧迁移）：消费面直接持有 *policy.Set。

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

// MethodPolicy 是规范树的单方法视图。
type MethodPolicy struct {
	Method, Service string
	Access          AccessLevel
	Permissions     []string
	AdminRoles      []string
	Scope           *ScopeRule
	RequestFields   []string
	IsStreaming     bool
}

// ViewOf 返回单方法视图；方法不存在时 ok=false，非规范树形返回错误。
func ViewOf(set *policy.Set, method string) (MethodPolicy, bool, error) {
	op, ok := set.Get(method)
	if !ok {
		return MethodPolicy{}, false, nil
	}
	mp, err := View(op)
	if err != nil {
		return MethodPolicy{}, true, fmt.Errorf("method %s: %w", op.ID, err)
	}
	return mp, true, nil
}

// Views 返回全量方法视图（按方法名排序）；首个非规范树形即错（生产装配
// 先过 AssertSemantic 聚合报错，此处供文档/golden 等已验证面消费）。
func Views(set *policy.Set) ([]MethodPolicy, error) {
	ops := set.Operations()
	out := make([]MethodPolicy, 0, len(ops))
	for _, op := range ops {
		mp, err := View(op)
		if err != nil {
			return nil, fmt.Errorf("method %s: %w", op.ID, err)
		}
		out = append(out, mp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method < out[j].Method })
	return out, nil
}

// ScopeRuleOf 返回 SERVER 面方法的 scope 规则（非 SERVER 或未声明返回 nil）。
func ScopeRuleOf(set *policy.Set, method string) *ScopeRule {
	mp, ok, err := ViewOf(set, method)
	if err != nil || !ok || mp.Access != AccessServer || mp.Scope == nil {
		return nil
	}
	rule := *mp.Scope
	return &rule
}

// AdminRolesOf 返回 SERVER 面方法允许的 admin 角色集（非 SERVER 面或未
// 声明返回 nil；nil 语义 = 不限角色，viewer 可调）。供 serverhttp 等
// 镜像消费点派生，禁止手写角色集。
func AdminRolesOf(set *policy.Set, method string) []string {
	mp, ok, err := ViewOf(set, method)
	if err != nil || !ok || mp.Access != AccessServer {
		return nil
	}
	return append([]string(nil), mp.AdminRoles...)
}
