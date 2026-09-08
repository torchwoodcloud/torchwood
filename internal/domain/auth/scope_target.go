package auth

import (
	"fmt"
	"regexp"
	"strings"
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

// ScopeTargets 是单次请求寻址到的目标资源实例（空 = 该方法不寻址具体实例）。
type ScopeTargets struct {
	DatabaseID string
	BucketID   string
}

func (t ScopeTargets) match(r ScopeResource, id string) bool {
	switch r {
	case ScopeDatabases:
		return t.DatabaseID != "" && t.DatabaseID == id
	case ScopeStorage:
		return t.BucketID != "" && t.BucketID == id
	}
	return false
}

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
			return fmt.Errorf("database id %q 必须匹配 ^[a-z][a-z0-9]{0,27}$", id)
		}
	case ScopeStorage:
		if !bucketIDPattern.MatchString(id) {
			return fmt.Errorf("bucket id %q 必须匹配 ^[0-9a-zA-Z_-]{1,64}$", id)
		}
	default:
		return fmt.Errorf("资源 %q 不支持实例级 scope", string(r))
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

// AllowsAPIKeyTargets 是 AllowsAPIKey 的目标感知扩展（T-02 匹配语义）：
//   - `*`/`all`、裸资源、`<res>.op` 行为与既有 AllowsAPIKey 完全一致
//     （向后兼容不变量，由回归测试锁定）；
//   - `<res>:<id>[.op]` 仅当请求寻址恰好命中该实例时放行；方法不寻址实例
//     （targets 零值）或寻址其他实例一律不放行。
//
// 未声明 scope 的方法（rule==nil）依旧一律拒绝。
func (s *PolicySet) AllowsAPIKeyTargets(fullMethod string, scopes []string, targets ScopeTargets) bool {
	rule := s.HasAPIKeyScope(fullMethod)
	if rule == nil {
		return false
	}
	for _, sc := range scopes {
		if sc == "*" || sc == "all" {
			return true
		}
		tok, ok := ParseScopeToken(sc)
		if !ok {
			continue
		}
		if tok.Resource != rule.Resource {
			continue
		}
		if tok.Op != "" && tok.Op != rule.Op {
			continue
		}
		if tok.TargetID != "" && !targets.match(rule.Resource, tok.TargetID) {
			continue
		}
		return true
	}
	return false
}
