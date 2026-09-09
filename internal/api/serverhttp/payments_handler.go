// Package serverhttp 之 payments 回调入口（v3 设计 §1.4 / 决策 D7）：
// 渠道 webhook 需要原始 body 验签，走裸 HTTP handler（不进 gRPC gateway
// 的 JSON 反序列化）。验签失败一律 401、不落任何行、不回执业务码；
// 渠道回执格式（Stripe 空 body / 微信 JSON / 支付宝纯文本 / iOS 空 body）
// 由 adapter CallbackAck 写出。
package serverhttp

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	apppayments "github.com/torchwoodcloud/torchwood/internal/app/payments"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

// maxCallbackBody 是渠道回调体上限（1 MiB）。
const maxCallbackBody = 1 << 20

// PaymentsHandler 是支付渠道回调 HTTP handler。
type PaymentsHandler struct {
	payments *apppayments.Payments
	logger   *slog.Logger
	// audit 是可选的拒绝审计 sink（M5 C6：验签失败落 denied 审计行）。
	audit audit.Repository
}

// NewPaymentsHandler creates the payments callback handler.
func NewPaymentsHandler(payments *apppayments.Payments, logger *slog.Logger, auditRepo audit.Repository) (*PaymentsHandler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return &PaymentsHandler{payments: payments, logger: logger, audit: auditRepo}, nil
}

// Register 挂载泛化回调路由 POST /v1/payments/callbacks/{provider}。
func (h *PaymentsHandler) Register(mux *runtime.ServeMux) {
	_ = mux.HandlePath("POST", "/v1/payments/callbacks/{provider}", h.callback)
}

func (h *PaymentsHandler) callback(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	provider := strings.ToLower(strings.TrimSpace(pathParams["provider"]))
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCallbackBody+1))
	if err != nil || len(raw) > maxCallbackBody {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if err := h.payments.HandleCallback(r.Context(), provider, r.Header, raw); err != nil {
		if errors.Is(err, domainpayments.ErrSignatureInvalid) {
			// M5 C6：验签失败补一条 deny 审计（IP 用对端地址：webhook 来源
			// 由渠道网段决定，XFF 可伪造，不采纳）。
			auditDenyFromHTTP(r, r.RemoteAddr, h.audit, h.logger,
				"/v1/payments/callbacks/"+provider, "signature_invalid")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if errors.Is(err, domainpayments.ErrProviderIndexMiss) {
			h.writeAckRetry(w, provider)
			return
		}
		h.logger.Error("payment callback processing failed", "provider", provider, "error", err)
		h.writeAck(w, provider, false)
		return
	}
	h.writeAck(w, provider, true)
}

func (h *PaymentsHandler) writeAck(w http.ResponseWriter, provider string, success bool) {
	status, contentType, body := h.payments.CallbackAck(provider, success)
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func (h *PaymentsHandler) writeAckRetry(w http.ResponseWriter, provider string) {
	_, contentType, body := h.payments.CallbackAck(provider, false)
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}
