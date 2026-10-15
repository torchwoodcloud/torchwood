package functions

// v4 JSON-RPC 2.0 服务面（docs/design/functions-v4.md §4）：函数常驻服务的
// 标准入口——验签中间件 + method 分发（invoke / cron.tick / event.document /
// http.request），复用 Mux 注册的 handler 与既有 adapter（dispatchInfo.rawBody
// 直通 TW_DATA 语义）。响应恒 200（函数成败在 result / error 对象）；验签
// 失败 401（传输层）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
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

// http.request 的 params 形状（与平台 HTTPRequestParams 同构）。
type httpRequestParams struct {
	Envelope *triggerEnvelope `json:"envelope"`
	BodyB64  string           `json:"body_b64,omitempty"`
}

// rpcRequestTimeout 是 v4 RPC 入口的 per-request 预算缺省（平台侧按函数
// 超时裁剪 ctx；此为函数侧兜底，防止注册了慢 handler 的服务被单请求挂死）。
const rpcRequestTimeout = 30 * time.Second

// serveRPCBody 解析并分发 JSON-RPC 请求（body 已由统一入口 NewServer 读取
// 并探测过形态）。
func serveRPCBody(w http.ResponseWriter, r *http.Request, body []byte, mux *Mux) {
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, "", &rpcError{Code: rpcCodeParseError, Message: "malformed json-rpc request: " + err.Error()})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeInvalidRequest, Message: "not a json-rpc 2.0 request"})
		return
	}

	ctx, cancel := context.WithTimeout(identityContext(r), rpcRequestTimeout)
	defer cancel()

	switch req.Method {
	case RPCMethodInvoke:
		serveRPCMain(ctx, w, &req, mux)
	case RPCMethodHTTPRequest:
		serveRPCHTTP(ctx, w, &req, mux)
	default:
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeMethodNotFound, Message: "unknown method " + req.Method})
	}
}

// serveRPCMain 处理 invoke：params 即 TW_DATA 语义，直通 mainAdapter。
func serveRPCMain(ctx context.Context, w http.ResponseWriter, req *rpcRequest, mux *Mux) {
	mux.mu.RLock()
	var handler contractHandler
	if mux.invoke != nil {
		handler = mainAdapter(mux.invoke)
	}
	mux.mu.RUnlock()
	if handler == nil {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeMethodNotFound, Message: "no Invoke handler registered"})
		return
	}

	res := handler.serveContract(ctx, &dispatchInfo{rawBody: req.Params})
	env, ok := res.envelope.(*mainEnvelope)
	if !ok {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeInternalError, Message: "unexpected contract envelope"})
		return
	}
	if !env.OK {
		code := rpcCodeServerRange
		if res.status == http.StatusBadRequest {
			code = rpcCodeInvalidParams
		}
		writeRPC(w, req.ID, &rpcError{Code: code, Message: env.Error})
		return
	}
	writeRPC(w, req.ID, env.Result)
}

// serveRPCHTTP 处理 http.request：params 组 dispatchInfo（envelope + body），
// 复用 fetchAdapter，fetchEnvelope 映射为 httpResult。
func serveRPCHTTP(ctx context.Context, w http.ResponseWriter, req *rpcRequest, mux *Mux) {
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
	mux.mu.RLock()
	fetch := mux.fetch
	mux.mu.RUnlock()
	if fetch == nil {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeMethodNotFound, Message: "no Fetch handler registered"})
		return
	}

	raw := params.BodyB64
	if decoded, err := base64.StdEncoding.DecodeString(params.BodyB64); err == nil {
		raw = string(decoded)
	}
	res := fetchAdapter{inner: fetch}.serveContract(ctx, &dispatchInfo{
		rawBody:  []byte(raw),
		envelope: params.Envelope,
	})
	env, ok := res.envelope.(*fetchEnvelope)
	if !ok {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeInternalError, Message: "unexpected contract envelope"})
		return
	}
	if !env.OK {
		writeRPC(w, req.ID, &rpcError{Code: rpcCodeServerRange, Message: "http request failed"})
		return
	}
	writeRPC(w, req.ID, httpResult{Status: env.Status, Headers: env.Headers, BodyB64: env.BodyB64})
}

// identityContext 从请求头组装执行身份（与契约循环同款头族；v4 下
// X-Tw-Execution-Id 头退役、id 走 JSON-RPC body——身份头仅剩 token/source/
// invoking-user/project 四件）。X-Tw-Protocol-Version 不匹配时宽容继续
// （平台升版不立即打断旧 FDK；日志观察点后续接）。
func identityContext(r *http.Request) context.Context {
	return WithIdentity(r.Context(), Identity{
		ExecutionToken: r.Header.Get(hdrExecutionToken),
		Source:         orDefault(r.Header.Get(hdrSource), "server"),
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
