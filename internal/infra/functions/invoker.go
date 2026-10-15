package functions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/ssrfguard"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// HTTPInvoker 是 domain Invoker 端口的 v4 生产实现（docs/design/
// functions-v4.md §4/§5）：对函数常驻 endpoint 发起 JSON-RPC 2.0 over
// 签名 HTTP 调用。
//
// 出站安全：client 必须经 ssrfguard 守卫构造（NewGuardedInvokerClient）；
// 测试经 NewHTTPInvoker 注入任意 client（httptest 回环地址会被守卫拦截）。
//
// 错误边界（端口契约的实现侧）：
//   - err 非 nil = 平台语义失败：连不通/超时（DeadlineExceeded）、
//     endpoint 非 2xx（Unavailable；429 映射 ResourceExhausted 保留退避
//     语义）、响应超限/协议不符（Unavailable）；
//   - err == nil = 送达成功，函数成败在 InvocationResult.FunctionError。
type HTTPInvoker struct {
	client *http.Client
	now    func() time.Time
}

// NewHTTPInvoker 以注入的 client 构造（生产装配请用 NewGuardedInvokerClient
// 产出的守卫 client；测试注入 http.DefaultTransport 系即可）。
func NewHTTPInvoker(client *http.Client) *HTTPInvoker {
	return &HTTPInvoker{client: client, now: time.Now}
}

// NewGuardedInvokerClient 返回生产出站 client：ssrfguard 拨号点校验、无
// 客户端级超时（预算由每次调用的 ctx 承载）。
//
// allowPrivateTargets 放行私网/回环 target（2026-10-15 私网 target 勘误，
// 配置 functions.invoke.allow_private_targets）：自托管形态下函数与 server
// 同处私网（fleetly 任务网成员挂靠 / k8s Service / 裸机内网）时由运营方
// 显式开启——平台级信任决策，非 per-request 开关；托管形态恒 false（默认）。
func NewGuardedInvokerClient(allowPrivateTargets bool) *http.Client {
	return ssrfguard.NewClient(ssrfguard.Options{AllowInsecure: allowPrivateTargets})
}

// Invoke 实现 Invoker 端口。
func (i *HTTPInvoker) Invoke(ctx context.Context, inv functions.Invocation) (*functions.InvocationResult, error) {
	body, err := i.marshalRequest(inv)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invoker: marshal rpc request: %v", err)
	}

	// 调用预算：函数超时透传为请求超时（0 = 不设，沿用调用方 ctx）。
	if inv.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(inv.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inv.TargetURL, bytes.NewReader(body))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invoker: build request: %v", err)
	}
	i.applyHeaders(req, inv, body)

	start := i.now()
	resp, err := i.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.Errorf(codes.DeadlineExceeded, "invoker: invoke %s: %v", inv.Method, ctx.Err())
		}
		return nil, status.Errorf(codes.Unavailable, "invoker: dial %s: %v", redactURL(inv.TargetURL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	durationMS := time.Since(start).Milliseconds()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, status.Errorf(codes.ResourceExhausted, "invoker: endpoint 429: %s", truncateTail(tail))
		}
		return nil, status.Errorf(codes.Unavailable, "invoker: endpoint status %d: %s", resp.StatusCode, truncateTail(tail))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, functions.MaxResultBodyBytes+1))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "invoker: read response: %v", err)
	}
	if len(raw) > functions.MaxResultBodyBytes {
		return nil, status.Errorf(codes.Unavailable, "invoker: response exceeds %d bytes", functions.MaxResultBodyBytes)
	}

	var rpcResp functions.RPCResponse
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return nil, status.Errorf(codes.Unavailable, "invoker: decode rpc response: %v", err)
	}
	result, err := mapResult(inv.Method, &rpcResp, durationMS)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "invoker: %v", err)
	}
	return result, nil
}

// marshalRequest 组装 JSON-RPC 请求 body：params 形状随 method（协议 §4.1；
// D10 后 RPC 轴仅 invoke/http.request 两 method——cron/事件走事件轴
// HTTPNotifier）。
func (i *HTTPInvoker) marshalRequest(inv functions.Invocation) ([]byte, error) {
	var params any
	switch inv.Method {
	case functions.MethodInvoke:
		if inv.Data != "" {
			params = json.RawMessage(inv.Data)
		}
	case functions.MethodHTTPRequest:
		bodyB64 := base64.StdEncoding.EncodeToString(inv.RawBody)
		if inv.RawBodyIsB64 {
			bodyB64 = string(inv.RawBody)
		}
		params = functions.HTTPRequestParams{Envelope: inv.TriggerEnvelope, BodyB64: bodyB64}
	default:
		return nil, fmt.Errorf("unknown method %q", inv.Method)
	}
	return json.Marshal(functions.RPCRequest{
		JSONRPC: "2.0",
		Method:  inv.Method,
		Params:  params,
		ID:      inv.ExecutionID,
	})
}

// applyHeaders 注入签名与身份头（传输与载荷分离，协议 §4）。空值身份
// 不发头（与旧分发面同口径：函数不得把缺省当匿名放行）。
func (i *HTTPInvoker) applyHeaders(req *http.Request, inv functions.Invocation, body []byte) {
	ts := i.now().Unix()
	entries := [][2]string{{"v1", functions.ComputeSignature(inv.TargetSecret, ts, body)}}
	if inv.TargetPreviousSecret != "" {
		entries = append(entries, [2]string{"v2", functions.ComputeSignature(inv.TargetPreviousSecret, ts, body)})
	}
	req.Header.Set(functions.HeaderSignature, functions.BuildSignatureHeader(ts, entries))
	req.Header.Set(functions.HeaderProtocolVersion, functions.ProtocolVersion)
	req.Header.Set("Content-Type", "application/json")
	if inv.ExecutionToken != "" {
		req.Header.Set(functions.HeaderExecutionToken, inv.ExecutionToken)
	}
	if inv.Source != "" {
		req.Header.Set(functions.HeaderSource, inv.Source)
	}
	if inv.InvokingUserID != "" {
		req.Header.Set(functions.HeaderInvokingUserID, inv.InvokingUserID)
	}
	if inv.ProjectID != "" {
		req.Header.Set(functions.HeaderProjectID, inv.ProjectID)
	}
}

// mapResult 把 JSON-RPC 响应映射为 InvocationResult：http.request 的
// result 解码为 HTTPResult（fetch 语义透传面）；其余 method 的 result
// 原样序列化为 Response JSON 文本；error 对象进 FunctionError（合法结果）。
func mapResult(method string, resp *functions.RPCResponse, durationMS int64) (*functions.InvocationResult, error) {
	out := &functions.InvocationResult{DurationMS: durationMS}
	if resp.Error != nil {
		out.FunctionError = resp.Error
		return out, nil
	}
	if resp.Result == nil {
		return out, nil
	}
	if method == functions.MethodHTTPRequest {
		raw, err := json.Marshal(resp.Result)
		if err != nil {
			return nil, fmt.Errorf("encode http result: %v", err)
		}
		var hr functions.HTTPResult
		if err := json.Unmarshal(raw, &hr); err != nil {
			return nil, fmt.Errorf("decode http result: %v", err)
		}
		out.StatusCode = hr.Status
		out.Headers = hr.Headers
		out.ResponseB64 = hr.BodyB64
		if decoded, err := base64.StdEncoding.DecodeString(hr.BodyB64); err == nil && utf8.Valid(decoded) {
			out.Response = string(decoded)
		}
		return out, nil
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %v", err)
	}
	out.Response = string(raw)
	return out, nil
}

// redactURL 从错误消息抹除 URL query（签名密钥不进 URL，但 query 可能
// 携带业务敏感参数；host 级定位信息保留）。
func redactURL(raw string) string {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '?' {
			return raw[:i]
		}
	}
	return raw
}

func truncateTail(b []byte) string {
	s := string(b)
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	return s
}
