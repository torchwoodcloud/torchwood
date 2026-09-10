// Package serverhttp 之函数触发器公开入口（P1 触发器模块，设计 §3）：
//
//	/f/{project_id}/{trigger_token}
//
// token（128bit 随机）即鉴权——不可猜即安全，不经 gRPC 拦截器链（无
// AuditInterceptor），执行记录 trigger_source/source_ip 即审计载体。平台只
// 路由不验签（K4）：请求以封套 {method, path, raw_query, headers(白名单),
// body} 透传进 TW_DATA，验签归函数代码（微信 SSV sha256 握手 + AES 解密，
// 密钥放 function_variables secret）。
//
// 双响应模式（二轮复审：微信 SSV 回调超时 1s×重试 3 次，纯同步大概率全超
// 时致事件丢失）：sync 同步执行透传响应；async_ack 先入队成功、后写 200
// （顺序红线——先 200 后入队的抖动窗口 = 事件永久丢失且平台无痕迹），入队
// 失败一律 5xx 让回调方重试。
package serverhttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TriggerInvoker 是触发器 handler 需要的最小 app 面（接口化便于单测，
// AuthValidator 同模式；*appfunctions.Functions 满足）。
type TriggerInvoker interface {
	GetHTTPTriggerByToken(ctx context.Context, projectID, token string) (*domainfunctions.Trigger, error)
	InvokeTrigger(ctx context.Context, cmd appfunctions.InvokeTriggerCommand) (*domainfunctions.ExecutionRecord, error)
	MaxTriggerBodyLimit(configured int) int
}

// TriggerIPLimiter 是 per-IP 固定窗口限频端口（infra/functions 提供 Redis
// 实现；接口化便于单测与消除 api→infra 直依赖）。
type TriggerIPLimiter interface {
	AllowTriggerIP(ctx context.Context, ip string) (allowed bool, retryAfter time.Duration, err error)
}

// FunctionTriggersHandler 是函数触发器公开 HTTP handler。
type FunctionTriggersHandler struct {
	invoker TriggerInvoker
	limiter TriggerIPLimiter
	trusted *interceptor.TrustedProxies
	logger  *slog.Logger
}

func NewFunctionTriggersHandler(invoker TriggerInvoker, limiter TriggerIPLimiter, cfg *config.AppConfig, logger *slog.Logger) (*FunctionTriggersHandler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	trusted, err := interceptor.ParseTrustedProxies(cfg.GetSecurity().GetTrustedProxies())
	if err != nil {
		return nil, err
	}
	return &FunctionTriggersHandler{invoker: invoker, limiter: limiter, trusted: trusted, logger: logger}, nil
}

// Register 挂载公开触发路由（网关同 mux，payments 先例）。
func (h *FunctionTriggersHandler) Register(mux *runtime.ServeMux) {
	_ = mux.HandlePath("GET", "/f/{project_id}/{trigger_token}", h.handle)
	_ = mux.HandlePath("POST", "/f/{project_id}/{trigger_token}", h.handle)
}

// handle 统一入口：限频 → token 查找 → GET 握手 / POST invoke。
func (h *FunctionTriggersHandler) handle(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	projectID := pathParams["project_id"]
	token := pathParams["trigger_token"]
	if projectID == "" || token == "" {
		h.finish(w, r, "", "", "", appfunctions.InvokeResultNotFound, http.StatusNotFound, "")
		return
	}

	// per-IP 限频先于 token 查找：探测流量不打 DB。配额可配（config
	// functions.trigger.http_ip_per_minute，默认 3000/min）。Redis 故障
	// fail-closed：503 让回调方重试（函数链路整体依赖 Redis，语义一致）。
	ip := h.clientIP(r)
	allowed, retryAfter, err := h.limiter.AllowTriggerIP(r.Context(), ip)
	if err != nil {
		h.logger.Warn("trigger ip rate limit check failed", slog.String("ip", ip), slog.String("error", err.Error()))
		h.finish(w, r, projectID, "", "", appfunctions.InvokeResultError, http.StatusServiceUnavailable, "")
		return
	}
	if !allowed {
		if retryAfter > 0 {
			w.Header().Set("Retry-After", retryAfterString(retryAfter))
		}
		h.finish(w, r, projectID, "", "", appfunctions.InvokeResultQuota, http.StatusTooManyRequests, "")
		return
	}

	// token 查找（project+token→启用中的 http 触发器）；未命中 404 不泄露
	// 存在性（跨项目探测、禁用触发器同形态）。
	trg, err := h.invoker.GetHTTPTriggerByToken(r.Context(), projectID, token)
	if err != nil {
		h.logger.Error("trigger lookup failed", slog.String("project_id", projectID), slog.String("error", err.Error()))
		h.finish(w, r, projectID, "", "", appfunctions.InvokeResultError, http.StatusInternalServerError, "")
		return
	}
	if trg == nil {
		h.finish(w, r, projectID, "", "", appfunctions.InvokeResultNotFound, http.StatusNotFound, "")
		return
	}

	// GET：仅 echo 握手（K4：回显不授予任何能力；不 invoke，探测行为可
	// 观测）；非 echo 模式按 method 不允许处理。
	if r.Method == http.MethodGet {
		if trg.Config.Handshake != domainfunctions.HandshakeEcho {
			w.Header().Set("Allow", "POST")
			h.finish(w, r, projectID, trg.FunctionID, trg.ID, appfunctions.InvokeResultError, http.StatusMethodNotAllowed, "")
			return
		}
		h.finish(w, r, projectID, trg.FunctionID, trg.ID, appfunctions.InvokeResultEcho, http.StatusOK, "")
		echoVal, _ := json.Marshal(r.URL.Query().Get("echostr"))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"echostr":`+string(echoVal)+`}`)
		return
	}
	h.invoke(w, r, trg, ip)
}

// invoke 处理 POST：读 body（LimitReader，超限 413）→ 构造封套 → sync 透传
// / async_ack 先入队后 200。
func (h *FunctionTriggersHandler) invoke(w http.ResponseWriter, r *http.Request, trg *domainfunctions.Trigger, ip string) {
	projectID, functionID, triggerID := trg.ProjectID, trg.FunctionID, trg.ID

	bodyLimit := h.invoker.MaxTriggerBodyLimit(trg.Config.EffectiveBodyLimit())
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(bodyLimit)+1))
	if err != nil {
		h.finish(w, r, projectID, functionID, triggerID, appfunctions.InvokeResultError, http.StatusBadRequest, "")
		return
	}
	if len(body) > bodyLimit {
		h.finish(w, r, projectID, functionID, triggerID, appfunctions.InvokeResultError, http.StatusRequestEntityTooLarge, "")
		return
	}

	data, envelope := buildTriggerEnvelope(r, body)
	cmd := appfunctions.InvokeTriggerCommand{
		ProjectID:      projectID,
		FunctionID:     functionID,
		Data:           data,
		Source:         trg.TriggerSource(),
		SourceIP:       ip,
		BodyLimitBytes: bodyLimit,
		// v3 §2.3/D10 恒填充：封套元数据 + 原始 body 随执行规格透传（body
		// 已过上方 413 校验）。runner fetch 风格还原 Request；main 风格重组
		// TW_DATA 与现状等价（app 不探测 runner 风格）。
		TriggerEnvelope: envelope,
		RawBody:         body,
	}

	if trg.Config.ResponseMode == domainfunctions.ResponseModeAsyncAck {
		// async_ack 顺序红线：先 Enqueue 成功、后写 200。InvokeTrigger 在
		// 入队失败时返回错误（执行记录标 failed），此处一律 5xx 让微信重试。
		cmd.Async = true
		if _, err := h.invoker.InvokeTrigger(r.Context(), cmd); err != nil {
			h.logger.Warn("async trigger enqueue failed",
				slog.String("project_id", projectID), slog.String("function_id", functionID),
				slog.String("trigger_id", triggerID), slog.String("error", err.Error()))
			h.finish(w, r, projectID, functionID, triggerID, appfunctions.InvokeResultError, http.StatusServiceUnavailable, "")
			return
		}
		h.finish(w, r, projectID, functionID, triggerID, appfunctions.InvokeResultOK, http.StatusOK, trg.Config.AckBody)
		return
	}

	// sync：同步执行（≤30s；ctx 超时 = 30s + 余量，网关 TimeoutHandler 60s
	// 兜底）。fetch 风格（v3 §2.2/D10）透传完整 HTTP 响应（status/headers/
	// body 原样）；main 风格照旧 200 + Response 文本。
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	rec, err := h.invoker.InvokeTrigger(ctx, cmd)
	if err != nil {
		h.finish(w, r, projectID, functionID, triggerID, invokeTriggerErrorResult(err), invokeTriggerErrorStatus(err), "")
		return
	}
	if rec != nil && rec.Status == domainfunctions.ExecutionStatusCompleted {
		// fetch 风格信号：rec.StatusCode ≥ 100 = 函数 HTTP status（executor
		// 链路回传；main 风格恒 0——退出码语义位）。完整透传 status/headers/
		// body（自定义状态码、二进制、content-type 从此可达）；失败语义不受
		// 影响（函数 HTTP 4xx/5xx 是合法一等结果，D10）。
		if rec.StatusCode >= 100 {
			h.finishTransparent(w, r, projectID, functionID, triggerID, rec)
			return
		}
		h.finish(w, r, projectID, functionID, triggerID, appfunctions.InvokeResultOK, http.StatusOK, rec.Response)
		return
	}
	resp := ""
	if rec != nil {
		resp = rec.Response
	}
	h.finish(w, r, projectID, functionID, triggerID, appfunctions.InvokeResultError, http.StatusBadGateway, resp)
}

// buildTriggerEnvelope 构造 TW_DATA 封套 {method, path, raw_query, headers,
// body, body_base64} 与封套元数据（v3 §2.3：TriggerEnvelope——不含 body，
// 随执行规格透传给 runner，fetch 风格还原 Request / main 风格重组 TW_DATA）。
// headers 白名单（统一小写键）：全部 x-*（大小写不敏感）+ content-type +
// wechatpay-* 前缀——白名单缺了验签头，「验签归函数」的前提就塌了；两形态
// 共用同一白名单。body 双通道：body 为 best-effort UTF-8 字符串（JSON 序列
// 化把非法字节替换为 U+FFFD），body_base64 恒在（无损，二进制 webhook 用）。
func buildTriggerEnvelope(r *http.Request, body []byte) (string, *domainfunctions.TriggerEnvelope) {
	headers := whitelistHeaders(r)
	envelope := map[string]any{
		"method":      r.Method,
		"path":        r.URL.Path,
		"raw_query":   r.URL.RawQuery,
		"headers":     headers,
		"body":        string(body),
		"body_base64": base64.StdEncoding.EncodeToString(body),
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		// map[string]any 序列化不会失败；防御分支。
		payload = []byte(`{}`)
	}
	return string(payload), &domainfunctions.TriggerEnvelope{
		Method:   r.Method,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Headers:  headers,
	}
}

// whitelistHeaders 抽取请求头的白名单子集（统一小写键、多值保留）：
// 全部 x-* + content-type + wechatpay-* 前缀。
func whitelistHeaders(r *http.Request) map[string][]string {
	headers := make(map[string][]string)
	for name, vals := range r.Header {
		ln := strings.ToLower(name)
		if strings.HasPrefix(ln, "x-") || ln == "content-type" || strings.HasPrefix(ln, "wechatpay-") {
			headers[ln] = vals
		}
	}
	return headers
}

// invokeTriggerErrorStatus 把 InvokeTrigger 错误映射为 HTTP 状态
// （DeadlineExceeded=504 对齐既有约定）。Unknown = 函数执行失败封套
// （dispatcher runner ok=false）→ 502 BadGateway：对回调方而言上游函数
// 失败即上游错误，与 rec.Status=failed 的 502 分支同语义（fetch/main 风格
// 一致，v3 §2.2）。
func invokeTriggerErrorStatus(err error) int {
	code := status.Code(err)
	switch code {
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.FailedPrecondition:
		return http.StatusConflict
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.Unknown:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func invokeTriggerErrorResult(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return appfunctions.InvokeResultTimeout
	}
	return appfunctions.InvokeResultError
}

// retryAfterString 把窗口剩余时长格式化为 Retry-After 秒数（向上取整，
// 至少 1 秒）。
func retryAfterString(d time.Duration) string {
	s := int64(d/time.Second) + 1
	if s < 1 {
		s = 1
	}
	return strconv.FormatInt(s, 10)
}

// transparentHeaderBlocklist 是 sync 完整透传的第二层过滤（v3 §2.2/D10）：
// runner 封套侧已滤一次（hop-by-hop + date/server），handler 再滤一层——
// 防御纵深：hop-by-hop 头（connection/keep-alive/transfer-encoding 等）
// 属传输层、不得由函数冒充；content-length 由本 handler 按实际 body 重算；
// host/date/server 是平台头。其余头（含 content-type/cache-control 等）
// 原样透传——完整 HTTP 响应语义的一部分。
var transparentHeaderBlocklist = map[string]bool{
	"connection":        true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"te":                true,
	"trailer":           true,
	"transfer-encoding": true,
	"upgrade":           true,
	"content-length":    true,
	"host":              true,
	"date":              true,
	"server":            true,
}

// finishTransparent 是 sync 模式的 fetch 风格完整透传（v3 §2.2 表「HTTP
// 触发器」行/D10）：status 原样、headers 透传（再滤一层 hop-by-hop +
// 补 Content-Length）、body 原字节（rec.ResponseB64 无损通道）。与 legacy
// finish 的差异：不强制 Cache-Control: no-store / Content-Type——响应头
// 归函数所有（透传是其目的）；指标与访问日志语义与 finish 完全一致。
// 持久化不感知：headers/body_b64 不落库（OQ7 收口语义，见
// domainfunctions.ExecutionRecord 注释）。
func (h *FunctionTriggersHandler) finishTransparent(w http.ResponseWriter, r *http.Request, projectID, functionID, triggerID string, rec *domainfunctions.ExecutionRecord) {
	appfunctions.ObserveInvoke(projectID, functionID, domainfunctions.TriggerTypeHTTP, appfunctions.InvokeResultOK)
	h.logger.Info("function trigger invoke",
		slog.String("method", r.Method),
		slog.String("project_id", projectID),
		slog.String("function_id", functionID),
		slog.String("trigger_id", triggerID),
		slog.String("result", appfunctions.InvokeResultOK),
		slog.Int("status", rec.StatusCode),
		slog.String("ip", h.clientIP(r)),
	)

	body, err := base64.StdEncoding.DecodeString(rec.ResponseB64)
	if err != nil {
		// ResponseB64 由 runner→dispatcher→executor 链路生成，失败仅防御。
		body = nil
	}
	for name, value := range rec.HTTPHeaders {
		if transparentHeaderBlocklist[strings.ToLower(name)] {
			continue
		}
		w.Header().Set(name, value)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(rec.StatusCode)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// finish 记指标 + 访问日志并写响应（无 body 时仅状态码）。日志不落 body
// 原文（安全红线：handler 不解析 body 内容、不落日志）。
func (h *FunctionTriggersHandler) finish(w http.ResponseWriter, r *http.Request, projectID, functionID, triggerID, result string, httpStatus int, body string) {
	appfunctions.ObserveInvoke(projectID, functionID, domainfunctions.TriggerTypeHTTP, result)
	h.logger.Info("function trigger invoke",
		slog.String("method", r.Method),
		slog.String("project_id", projectID),
		slog.String("function_id", functionID),
		slog.String("trigger_id", triggerID),
		slog.String("result", result),
		slog.Int("status", httpStatus),
		slog.String("ip", h.clientIP(r)),
	)
	if result == appfunctions.InvokeResultEcho {
		return // echo 的响应体由调用方写出（no-store）。
	}
	w.Header().Set("Cache-Control", "no-store")
	if body != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_, _ = io.WriteString(w, body)
		return
	}
	w.WriteHeader(httpStatus)
}

func (h *FunctionTriggersHandler) clientIP(r *http.Request) string {
	return h.trusted.ResolveClientIP(
		interceptor.PeerIPFromAddr(r.RemoteAddr),
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-Ip"),
	)
}
