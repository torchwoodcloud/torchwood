package interceptor

import (
	"encoding/json"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 管理面（Console 会话与 API Key/CLI/SDK 通道）写操作的请求摘要：结构化
// （JSONB metadata.request）、脱敏、有上限。client 数据面（文档 CRUD 等）
// 的审计载体是 action 级 + 事件流，不记录请求内容。
const (
	// auditRequestSummaryMaxBytes 摘要整体上限：超出时按 rune 边界截断并
	// 加后缀标记（截断产物不再是合法 JSON，属预期——保真优先于可解析）。
	auditRequestSummaryMaxBytes = 8 << 10
	// auditStringMaxRunes 单字符串截断（覆盖 base64 后的 bytes 字段，如
	// Functions 部署包：protojson 渲染为 base64 字符串，统一走此上限）。
	auditStringMaxRunes = 1024
	// auditArrayMaxItems 数组截断上限。
	auditArrayMaxItems    = 100
	auditTruncationSuffix = "…(truncated)"
)

// auditSummaryNamespaces：记录请求摘要的方法命名空间（管理面）。
var auditSummaryNamespaces = []string{
	"/torchwood.server.v1.",
	"/torchwood.console.v1.",
}

// auditReadMethodPrefixes：读方法跳过（读请求内容只有噪声）；未知动词一律
// 记录——偏向多记（漏记不可补，多记可过滤）。
var auditReadMethodPrefixes = []string{
	"List", "Get", "Health", "Count", "Search", "Stats", "Describe", "Ping", "Watch",
}

// auditSensitiveFieldPatterns：字段名归一（小写、去 _/-）后 contains 命中即
// 整值打码。刻意不含裸 "key"（会误伤 sort_key/labels.key 等业务字段）。
var auditSensitiveFieldPatterns = []string{
	"password", "passwd", "secret", "token", "credential", "authorization",
	"cookie", "apikey", "privatekey", "signature", "sessionid",
}

// auditRequestSummary 返回脱敏后的请求摘要 JSON；不满足记录条件（非管理面、
// 读方法、非 proto 消息、序列化失败）返回空串。
//
// 序列化用 protojson（UseProtoNames：与 API JSON 的 snake_case 一致）：
// 未设置字段天然省略——全 optional 的更新请求（如 UpdateFunctionRequest）
// 摘要即"本次被修改的字段集"。
func auditRequestSummary(fullMethod string, req any) string {
	if !auditSummaryEligible(fullMethod) {
		return ""
	}
	msg, ok := req.(proto.Message)
	if !ok || msg == nil {
		return ""
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		return ""
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return ""
	}
	sanitized := sanitizeAuditValue(tree, 0)
	out, err := json.Marshal(sanitized)
	if err != nil {
		return ""
	}
	if len(out) <= auditRequestSummaryMaxBytes {
		return string(out)
	}
	// 按 UTF-8 边界回退：continuation byte (10xxxxxx) 不做切点。
	cut := auditRequestSummaryMaxBytes
	for cut > 0 && out[cut]&0xC0 == 0x80 {
		cut--
	}
	return string(out[:cut]) + auditTruncationSuffix
}

// auditSummaryEligible 判定方法是否记录请求摘要：管理面命名空间 + 非读动词。
func auditSummaryEligible(fullMethod string) bool {
	ns := false
	for _, prefix := range auditSummaryNamespaces {
		if strings.HasPrefix(fullMethod, prefix) {
			ns = true
			break
		}
	}
	if !ns {
		return false
	}
	// FullMethod 形如 /torchwood.server.v1.UsersService/ListUsers：
	// 取最后一个 "/" 之后的方法名。
	idx := strings.LastIndex(fullMethod, "/")
	if idx < 0 || idx+1 >= len(fullMethod) {
		return false
	}
	verb := fullMethod[idx+1:]
	for _, prefix := range auditReadMethodPrefixes {
		if strings.HasPrefix(verb, prefix) {
			return false
		}
	}
	return true
}

// sanitizeAuditValue 递归脱敏任意 JSON 树（来自 protojson 反序列化）：
// map 键命中敏感模式 → 值整体打码；字符串超限截断；数组超限截断并留标记。
func sanitizeAuditValue(v any, depth int) any {
	if depth > 8 {
		return "(max depth)"
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if auditFieldSensitive(k) {
				out[k] = "[REDACTED]"
				continue
			}
			out[k] = sanitizeAuditValue(val, depth+1)
		}
		return out
	case []any:
		limit := auditArrayMaxItems
		if len(t) < limit {
			limit = len(t)
		}
		out := make([]any, 0, limit+1)
		for i := 0; i < limit; i++ {
			out = append(out, sanitizeAuditValue(t[i], depth+1))
		}
		if len(t) > limit {
			out = append(out, "(+more items truncated)")
		}
		return out
	case string:
		r := []rune(t)
		if len(r) > auditStringMaxRunes {
			return string(r[:auditStringMaxRunes]) + auditTruncationSuffix
		}
		return t
	default:
		return v
	}
}

// auditFieldSensitive 归一化字段名后与敏感模式做 contains 匹配。
func auditFieldSensitive(name string) bool {
	norm := strings.ToLower(name)
	norm = strings.ReplaceAll(norm, "_", "")
	norm = strings.ReplaceAll(norm, "-", "")
	for _, pattern := range auditSensitiveFieldPatterns {
		if strings.Contains(norm, pattern) {
			return true
		}
	}
	return false
}

// auditClientChannel 由凭证类型 + UA 自报身份推导调用通道，落
// metadata.client（结构化，供"CLI/SDK/Console 做的"这类查询消费）：
//   - session → console（Console admin 会话，浏览器 UA）；
//   - execution → function（函数执行身份）；
//   - token → user（端用户 JWT，client 面）；
//   - api_key → UA 前缀 torchwood-cli/torchwood-sdk* → cli/sdk，否则 api。
//
// CLI/SDK 经 SDK 的 WithUserAgent 显式自报（grpc 会追加自身 token，取首段）。
func auditClientChannel(p *shared.Principal, userAgent string) map[string]any {
	if p == nil {
		return nil
	}
	switch p.CredentialType {
	case shared.CredentialTypeSession:
		return map[string]any{"channel": "console"}
	case shared.CredentialTypeExecution:
		return map[string]any{"channel": "function"}
	case shared.CredentialTypeToken:
		return map[string]any{"channel": "user"}
	case shared.CredentialTypeAPIKey:
		product, version := parseSelfReportedAgent(userAgent)
		switch {
		case product == "torchwood-cli":
			return map[string]any{"channel": "cli", "product": product, "version": version}
		case strings.HasPrefix(product, "torchwood-sdk"):
			return map[string]any{"channel": "sdk", "product": product, "version": version}
		default:
			return map[string]any{"channel": "api"}
		}
	default:
		return nil
	}
}

// parseSelfReportedAgent 解析 UA 首段的 "product/version" 自报身份；
// 无自报（grpc-go/浏览器等）返回空 product。
func parseSelfReportedAgent(userAgent string) (product, version string) {
	first := userAgent
	if idx := strings.IndexByte(userAgent, ' '); idx >= 0 {
		first = userAgent[:idx]
	}
	product, version, ok := strings.Cut(first, "/")
	if !ok {
		return "", ""
	}
	return product, version
}
