package client

import (
	"context"
	"encoding/json"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
)

// RuntimeVarsService 封装 Client API 的运行时变量拉取面（docs/design/
// runtime-vars.md §2.3）：单一 GetRuntimeVars 按集合全量快照，无单 key 读/
// 分页/跨集合聚合（D4/D14）。SDK 不内置 TTL 缓存（设计 §7 明确不做），轮询
// 口径：建议间隔 TTL ≥ 60s，透传上次响应的 etag（命中则 unchanged=true 且
// vars 为空，常态轮询仅几十字节）；收到 404 保留 last-known-good 缓存
// （visibility 翻转 private 或集合删除，D15）。
type RuntimeVarsService struct{ c *Client }

// GetRuntimeVarsOptions 是 GetRuntimeVars 的可选参数。
type GetRuntimeVarsOptions struct {
	// ProjectID 是匿名唯一可靠寻址；为空时回落 X-Torchwood-Project 头 →
	// 登录 Principal（handler 三级回落链）。
	ProjectID string
	// ETag 透传上次响应的 etag；命中则服务端短路返回 unchanged=true（vars
	// 为空）。不透明 token "{epoch}:{revision}"（D2），调用方不要解析。
	ETag string
}

// GetRuntimeVars 拉取指定集合的全量快照（public 集匿名可读；private 集需
// 有效登录 Principal，匿名返回 NotFound）。unchanged=true 时 vars 为空、
// etag 仍为当前值——调用方应继续沿用本地缓存。
func (t *RuntimeVarsService) GetRuntimeVars(ctx context.Context, varSetID string, opts *GetRuntimeVarsOptions) (*clientv1.GetRuntimeVarsResponse, error) {
	req := &clientv1.GetRuntimeVarsRequest{VarSetId: varSetID}
	if opts != nil {
		req.ProjectId = opts.ProjectID
		req.Etag = opts.ETag
	}
	return t.c.runtimeVars.GetRuntimeVars(ctx, req)
}

// ---- RuntimeVarValue typed 取值器 ----
// oneof 让类型锚点在编译期可判（D3）；以下便捷函数按类型取值，第二返回值
// ok=false 表示 value 为 nil 或 oneof 不在该分支（调用方回落缺省值）。

// RuntimeVarValueAsString 取 string 分支。
func RuntimeVarValueAsString(v *clientv1.RuntimeVarValue) (string, bool) {
	if v == nil {
		return "", false
	}
	s, ok := v.Kind.(*clientv1.RuntimeVarValue_StringValue)
	return s.StringValue, ok
}

// RuntimeVarValueAsInteger 取 integer 分支（值域 ±2^53−1，服务端已拦）。
func RuntimeVarValueAsInteger(v *clientv1.RuntimeVarValue) (int64, bool) {
	if v == nil {
		return 0, false
	}
	i, ok := v.Kind.(*clientv1.RuntimeVarValue_IntegerValue)
	return i.IntegerValue, ok
}

// RuntimeVarValueAsFloat 取 float 分支。
func RuntimeVarValueAsFloat(v *clientv1.RuntimeVarValue) (float64, bool) {
	if v == nil {
		return 0, false
	}
	f, ok := v.Kind.(*clientv1.RuntimeVarValue_FloatValue)
	return f.FloatValue, ok
}

// RuntimeVarValueAsBool 取 bool 分支。
func RuntimeVarValueAsBool(v *clientv1.RuntimeVarValue) (bool, bool) {
	if v == nil {
		return false, false
	}
	b, ok := v.Kind.(*clientv1.RuntimeVarValue_BoolValue)
	return b.BoolValue, ok
}

// RuntimeVarValueAsJSON 将 json_value 文本解析为通用值（map/slice/number/…）
// 返回。写路径已保证合法 JSON 且嵌套 ≤100，解析失败仅理论存在（ok=false）。
func RuntimeVarValueAsJSON(v *clientv1.RuntimeVarValue) (any, bool) {
	if v == nil {
		return nil, false
	}
	j, ok := v.Kind.(*clientv1.RuntimeVarValue_JsonValue)
	if !ok {
		return nil, false
	}
	var out any
	if err := json.Unmarshal([]byte(j.JsonValue), &out); err != nil {
		return nil, false
	}
	return out, true
}
