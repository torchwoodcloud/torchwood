package auth

import (
	"fmt"
	"regexp"
	"strings"

	gscope "github.com/lynx-go/grpcapi/policy/scope"
)

// 本文件实现资源级 scope（安全审计 2026-09-08 T-02）：
//
//	<res>              = 该服务全项目权限（既有语义，行为不变）
//	<res>.read/.write  = 该服务全项目单向权限（既有语义，行为不变）
//	<res>:<id>         = 限定到单个资源实例（读写）
//	<res>:<id>.read/.write = 单个资源实例的单向权限
//
// 目前仅 databases / storage 两类资源可资源级限定（ScopeAddressable）；
// 其余资源携带 ':' 视为非法 scope（创建期 400，执行期不匹配——fail-closed）。
// 目标实例 ID 在请求寻址点提取（gRPC 拦截器经 getter / serverhttp 经路径
// 参数），无目标的方法（ListDatabases/ListBuckets/CreateBucket 等全集型）
// 对资源限定 scope 一律 403。

// scopeToken 是 scope 字符串的解析结果。
type scopeToken struct {
	Resource ScopeResource
	Op       ScopeOp // 空串 = 读写皆可
	TargetID string  // 空串 = 无资源限定（既有形态）
}

var (
	// databaseIDPattern 与 pkg/ident.ValidateSchemaResourceID 同源
	// （^[a-z][a-z0-9]{0,27}$，物理表名前缀安全字符集）。
	databaseIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,27}$`)
	// bucketIDPattern 与 serverhttp validBucketIDPattern 同源（不含 ':' 与
	// '.'，保证 scope 语法可无损往返）。
	bucketIDPattern = regexp.MustCompile(`^[0-9a-zA-Z_-]{1,64}$`)
)

// ScopeAddressable 报告资源是否定义了实例级限定语法。
func ScopeAddressable(r ScopeResource) bool {
	return r == ScopeDatabases || r == ScopeStorage
}

// ValidateScopeTargetID 校验资源限定段的目标 ID 格式（创建期 400 的依据）。
func ValidateScopeTargetID(r ScopeResource, id string) error {
	switch r {
	case ScopeDatabases:
		if !databaseIDPattern.MatchString(id) {
			return fmt.Errorf("database id %q must match ^[a-z][a-z0-9]{0,27}$", id)
		}
	case ScopeStorage:
		if !bucketIDPattern.MatchString(id) {
			return fmt.Errorf("bucket id %q must match ^[0-9a-zA-Z_-]{1,64}$", id)
		}
	default:
		return fmt.Errorf("resource %q does not support instance-level scope", string(r))
	}
	return nil
}

// ParseScopeToken 解析 scope 字符串；返回 false 表示语法非法（含未知资源、
// 未知方向、不可寻址资源携带 ':'、目标 ID 格式非法）。执行期对非法 token
// 一律不匹配（fail-closed）。
//
// 方向后缀附着在最后一段：无目标形态附着在资源段（databases.read），实例
// 限定形态附着在目标段（databases:blog.read）。
func ParseScopeToken(s string) (scopeToken, bool) {
	opSuffix := func(seg string) (string, ScopeOp, bool) {
		dot := strings.LastIndex(seg, ".")
		if dot < 0 {
			return seg, "", true
		}
		switch seg[dot+1:] {
		case string(ScopeRead):
			return seg[:dot], ScopeRead, true
		case string(ScopeWrite):
			return seg[:dot], ScopeWrite, true
		case string(ScopeAdmin):
			return seg[:dot], ScopeAdmin, true
		default:
			return "", "", false
		}
	}
	head, target := s, ""
	hasTarget := false
	if idx := strings.Index(s, ":"); idx >= 0 {
		head, target = s[:idx], s[idx+1:]
		hasTarget = target != ""
		if !hasTarget {
			return scopeToken{}, false // "databases:" 空目标非法
		}
	}
	var tok scopeToken
	var ok bool
	if head, tok.Op, ok = opSuffix(head); !ok {
		return scopeToken{}, false
	}
	res := head
	if hasTarget {
		// 目标段的 ID 字符集不含 '.'，其后缀必为方向（或非法）。
		if target, tok.Op, ok = opSuffix(target); !ok {
			return scopeToken{}, false
		}
	}
	validRes := false
	for _, r := range AllScopeResources {
		if ScopeResource(res) == r {
			tok.Resource, validRes = r, true
			break
		}
	}
	if !validRes {
		return scopeToken{}, false
	}
	if target == "" {
		return tok, true
	}
	// 资源限定段：仅可寻址资源 + 目标 ID 格式合法。
	if !ScopeAddressable(tok.Resource) {
		return scopeToken{}, false
	}
	if err := ValidateScopeTargetID(tok.Resource, target); err != nil {
		return scopeToken{}, false
	}
	tok.TargetID = target
	return tok, true
}

// AllowsAPIKey 判定 scope 集是否放行方法声明的 scope 规则（serverhttp/WS
// 等镜像面消费；gRPC 链由库 authz 拦截器按同一规范树求值）。语义全部委托
// grpcapi/policy/scope 实现，与五档树的 any_of[scope(res:op), resource(res)]
// 组间 OR 同构（单一语义源）：
//   - 操作门 Set.Satisfies：`*`/`all`、裸资源、res:op 命中即过；
//   - 实例门 GrantsInstance：实例 token（<res>:<id>）仅当请求寻址命中该实例；
//     操作级 token（read/write/admin）不授予实例寻址（库语义，torchwood T-02
//     逐 token 合取语义中 `<res>:<id>.op` 单 token 形态由此收窄，见迁移报告）。
//
// 未声明 scope 的方法（rule==nil）依旧一律拒绝；target 空串 = 请求不寻址
// 实例（实例 token 恒不匹配）。
func AllowsAPIKey(rule *ScopeRule, scopes []string, target string) bool {
	if rule == nil {
		return false
	}
	set := gscope.Set(scopes)
	if set.Satisfies(rule.Resource, string(rule.Op)) {
		return true
	}
	return gscope.GrantsInstance(set, rule.Resource, target, true)
}
