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
	"github.com/torchwoodcloud/torchwood/pkg/config"
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

	cmd := appfunctions.InvokeTriggerCommand{
		ProjectID:      projectID,
		FunctionID:     functionID,
		Data:           buildTriggerEnvelope(r, body),
		Source:         trg.TriggerSource(),
		SourceIP:       ip,
		BodyLimitBytes: bodyLimit,
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
	// 兜底），函数响应透传。
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	rec, err := h.invoker.InvokeTrigger(ctx, cmd)
	if err != nil {
		h.finish(w, r, projectID, functionID, triggerID, invokeTriggerErrorResult(err), invokeTriggerErrorStatus(err), "")
		return
	}
	if rec != nil && rec.Status == domainfunctions.ExecutionStatusCompleted {
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
// body, body_base64}。headers 白名单（统一小写键）：全部 x-*（大小写不敏感）
// + content-type + wechatpay-* 前缀——白名单缺了验签头，「验签归函数」的
// 前提就塌了。body 双通道：body 为 best-effort UTF-8 字符串（JSON 序列化把
// 非法字节替换为 U+FFFD），body_base64 恒在（无损，二进制 webhook 用）。
func buildTriggerEnvelope(r *http.Request, body []byte) string {
	headers := make(map[string][]string)
	for name, vals := range r.Header {
		ln := strings.ToLower(name)
		if strings.HasPrefix(ln, "x-") || ln == "content-type" || strings.HasPrefix(ln, "wechatpay-") {
			headers[ln] = vals
		}
	}
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
		return `{}`
	}
	return string(payload)
}

// invokeTriggerErrorStatus 把 InvokeTrigger 错误映射为 HTTP 状态
// （DeadlineExceeded=504 对齐既有约定）。
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
