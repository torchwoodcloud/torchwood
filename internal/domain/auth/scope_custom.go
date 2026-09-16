package auth

import (
	"regexp"
	"strings"
)

// 自定义服务 scope（跨系统标签机制，messageloop Admin API Key 设计 §2.6 / T2）：
//
//	scope   := <service> "." <name>
//	service := [a-z][a-z0-9-]{1,31}     // 服务命名空间，≥2 字符
//	name    := [a-z0-9_.-]{1,40}        // 语义归服务自己解释
//	总长 ≤ 64；全小写；不允许空段（双点 / 前导点 / 尾点）
//
// TW 只做语法校验与存储，不解释服务 scope 语义：自定义 scope 在
// PolicySet.AllowsAPIKeyTargets 中永不匹配任何 TW 方法（ParseScopeToken
// 拒绝 → fail-closed 跳过），仅作为不透明标签随 key 存储并经 whoami /
// GetAPIKey 原样下发，供外部系统按自身前缀过滤消费（如 mlbridge 只认
// "messageloop." 前缀）。
//
// 内建保护（不可覆盖语义）：service 段不得命中 TW 内建 scope 域（资源词表
// AllScopeResources 与保留别名 all）——无前缀形态（users.read、databases:blog
// 等）是 TW 自己的词表且受 TW 治理，内建命名空间不可被自定义 scope 占用，
// 否则 "users.anything" 这类串会被误读为进入 users 语义域。

const (
	// maxCustomScopeLen 是自定义 scope 总长上限（含 service 段、点与 name 段）。
	maxCustomScopeLen = 64
)

var (
	// customScopeServiceRe：小写字母开头，2–32 个 [a-z0-9-]。
	customScopeServiceRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	// customScopeNameRe：1–40 个 [a-z0-9_.-]；空段由 ParseCustomScope 额外拒绝。
	customScopeNameRe = regexp.MustCompile(`^[a-z0-9_.-]{1,40}$`)
)

// ParseCustomScope 解析 <service>.<name> 自定义 scope（按首个 '.' 切分，
// name 段可含点）。ok=false 表示不属于自定义语法——可能是内建形态（调用方
// 应先尝试内建解析）或非法串。
//
// 除正则语法外施加三条结构性约束：
//   - 总长 ≤ 64；
//   - 无空段：拒绝 ".."（连续点）、name 段以 '.' 开头、以 '.' 结尾
//     （"svc..x" / "svc..x.y" / "svc.x." 等）；
//   - 内建保护：service 段不得命中内建 scope 域（IsBuiltinScopeNamespace）。
func ParseCustomScope(s string) (service, name string, ok bool) {
	if len(s) > maxCustomScopeLen {
		return "", "", false
	}
	dot := strings.Index(s, ".")
	if dot <= 0 || dot == len(s)-1 {
		return "", "", false // 无点 / 空 service / 空 name
	}
	service, name = s[:dot], s[dot+1:]
	if strings.Contains(s, "..") || strings.HasPrefix(name, ".") {
		return "", "", false // 空段（双点 / 前导点）
	}
	if !customScopeServiceRe.MatchString(service) || !customScopeNameRe.MatchString(name) {
		return "", "", false
	}
	if IsBuiltinScopeNamespace(service) {
		return "", "", false // 内建域不可被自定义 scope 占用
	}
	return service, name, true
}

// IsBuiltinScopeNamespace 报告词是否属于 TW 内建 scope 域：资源词表
// （AllScopeResources，随词表演进自动扩展）+ 通配别名 all。内建域由 TW
// 治理，不可被自定义 scope 的 service 段覆盖。
func IsBuiltinScopeNamespace(word string) bool {
	if word == "all" {
		return true
	}
	for _, r := range AllScopeResources {
		if string(r) == word {
			return true
		}
	}
	return false
}
