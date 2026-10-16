package functions

// v4 JSON-RPC 2.0 服务面（RPC 轴，docs/design/functions-v4.md §4.1）：函数
// 常驻服务的标准入口之一——验签中间件 + method 分发（invoke / http.request），
// 直通 Mux 注册的 handler。响应恒 200（送达成功；函数成败在 result / error
// 对象）；验签失败 401（传输层，verify.go）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"
)

// JSON-RPC method 词表（与平台 domain protocol.go 同值；D10 双轴增补后
// RPC 轴仅两 method——cron/事件走事件轴 CloudEvents，见 cloudevents.go）。
const (
	RPCMethodInvoke      = "invoke"
	RPCMethodHTTPRequest = "http.request"
)

// JSON-RPC 错误码（spec §5.1 + server 区段）。
const (
	rpcCodeParseError     = -32700
	rpcCodeInvalidRequest = -32600
	rpcCodeMethodNotFound = -32601
	rpcCodeInvalidParams  = -32602
	rpcCodeInternalError  = -32603
	rpcCodeServerRange    = -32000
)

// 身份投影头（协议 §4：传输与载荷分离）。X-Tw-Execution-Id /
// X-Tw-Trigger-Envelope / X-Tw-Timeout-Seconds 三头已随 v4 退役——execution
// id 走 JSON-RPC body、触发器封套走 http.request params、超时由调用方 ctx
// 承载。未知 x-tw-* 头一律忽略（协议演进宪法，见包文档）。
const (
	hdrExecutionToken = "X-Tw-Execution-Token" // #nosec G101 -- 头名常量非凭证（与平台 protocol.go 同口径）
	hdrSource         = "X-Tw-Source"
	hdrInvokingUserID = "X-Tw-Invoking-User-Id"
	hdrProjectID      = "X-Tw-Project-Id"
)

// serverSourceFallback 是 x-tw-source 缺省回落（事件轴只发 token 头，恒落此值）。
const serverSourceFallback = "server"

// rpcRequest / rpcResponse 是 wire 形态。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      string          `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      string    `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

// httpResult 是 http.request 的 result 形状（与平台 HTTPResult 同构）。
type httpResult struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	BodyB64 string            `json:"body_b64,omitempty"`
}

// triggerEnvelope 是触发器封套元数据（http.request params.envelope 的 wire
// 形状；形状对照 internal/domain/functions TriggerEnvelope——只读对齐，SDK
// 自持结构体）。额外字段一律忽略（协议演进宪法）。
type triggerEnvelope struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	RawQuery string              `json:"raw_query"`
	Headers  map[string][]string `json:"headers"`
}

// httpRequestParams 是 http.request 的 params 形状（与平台 HTTPRequestParams
// 同构）。
type httpRequestParams struct {
	Envelope *triggerEnvelope `json:"envelope"`
	BodyB64  string           `json:"body_b64,omitempty"`
}

// rpcRequestTimeout 是 v4 RPC 入口的 per-request 预算缺省（平台侧按函数
// 超时裁剪 ctx；此为函数侧兜底，防止注册了慢 handler 的服务被单请求挂死）。
const rpcRequestTimeout = 30 * time.Second

// identityContext 从请求头 + 载荷关联键 + 部署 env 组装执行身份（六件）：
// token / source / invoking-user / project 四件走身份头（source 缺省回落
// "server"），executionID 是本次执行的关联键（RPC 轴 = JSON-RPC id，平台
// 白送的 invocation 关联），apiBaseURL 是平台 API base（server 构建期读入
// 的 TW_API_BASE_URL，每请求恒定）。X-Tw-Protocol-Version 不匹配时宽容继续
// （平台升版不立即打断旧 FDK；日志观察点后续接）。
func identityContext(r *http.Request, executionID, apiBaseURL string) context.Context {
	return WithIdentity(r.Context(), Identity{
		ExecutionToken: r.Header.Get(hdrExecutionToken),
		APIBaseURL:     apiBaseURL,
		ExecutionID:    executionID,
		Source:         orDefault(r.Header.Get(hdrSource), serverSourceFallback),
		InvokingUserID: r.Header.Get(hdrInvokingUserID),
		ProjectID:      r.Header.Get(hdrProjectID),
	})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// formatPanic 归一 panic 现场为 error 对象消息（含栈，平台侧 ≤1MiB 读取上
// 限兜底）。
func formatPanic(rec any) string {
	return fmt.Sprintf("panic: %v\n\n%s", rec, debug.Stack())
}

// serveRPC 解析并分发 JSON-RPC 请求（body 已由统一入口 NewServer 读取并探
// 测过形态）。
func (s *server) serveRPC(w http.ResponseWriter, r *http.Request, body []byte) {
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, "", &rpcError{Code: rpcCodeParseError, Message: "malformed json-rpc request: " + err.Error()})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeInvalidRequest, Message: "not a json-rpc 2.0 request"})
		return
	}

	ctx, cancel := context.WithTimeout(identityContext(r, req.ID, s.apiBaseURL), rpcRequestTimeout)
	defer cancel()

	switch req.Method {
	case RPCMethodInvoke:
		s.serveRPCMain(ctx, w, &req)
	case RPCMethodHTTPRequest:
		s.serveRPCHTTP(ctx, w, &req)
	default:
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeMethodNotFound, Message: "unknown method " + req.Method})
	}
}

// serveRPCMain 处理 invoke：params 即函数载荷（原 TW_DATA JSON，缺省补空对
// 象零值安全），直通 Mux.Invoke 注册的 handler。
func (s *server) serveRPCMain(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	invoke := s.mux.snapshot().invoke
	if invoke == nil {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeMethodNotFound, Message: "no Invoke handler registered"})
		return
	}
	data := req.Params
	if len(data) == 0 {
		data = json.RawMessage("{}")
	} else if !json.Valid(data) {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeInvalidParams, Message: "invoke params: invalid JSON"})
		return
	}

	defer func() {
		if rec := recover(); rec != nil {
			writeRPC(w, req.ID, &rpcError{Code: rpcCodeServerRange, Message: formatPanic(rec)})
		}
	}()
	out, err := invoke(ctx, data)
	if err != nil {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeServerRange, Message: err.Error()})
		return
	}
	raw, err := json.Marshal(out)
	if err != nil {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeInternalError, Message: "handler result is not JSON-serializable: " + err.Error()})
		return
	}
	writeRPC(w, req.ID, json.RawMessage(raw))
}

// serveRPCHTTP 处理 http.request：params 组触发器封套 + body，还原真请求交
// Fetch handler，响应捕获为 httpResult（status/headers/body_b64）。
func (s *server) serveRPCHTTP(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	var params httpRequestParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeRPC(w, req.ID, &rpcError{Code: rpcCodeInvalidParams, Message: "http.request params: " + err.Error()})
			return
		}
	}
	if params.Envelope == nil {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeInvalidParams, Message: "http.request params: missing envelope"})
		return
	}
	fetch := s.mux.snapshot().fetch
	if fetch == nil {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeMethodNotFound, Message: "no Fetch handler registered"})
		return
	}

	raw := params.BodyB64
	if decoded, err := base64.StdEncoding.DecodeString(params.BodyB64); err == nil {
		raw = string(decoded)
	}
	res, err := invokeFetch(ctx, fetch, params.Envelope, []byte(raw))
	if err != nil {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeServerRange, Message: "http request failed"})
		return
	}
	writeRPC(w, req.ID, res)
}

// writeRPC 恒写 200（送达成功；函数成败在 body）：payload 为 *rpcError
// 走 error 对象，其余（json.RawMessage / httpResult / nil）走 result。
func writeRPC(w http.ResponseWriter, id string, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	resp := rpcResponse{JSONRPC: "2.0", ID: id}
	if e, ok := payload.(*rpcError); ok {
		resp.Error = e
	} else {
		resp.Result = payload
	}
	_ = json.NewEncoder(w).Encode(resp)
}
