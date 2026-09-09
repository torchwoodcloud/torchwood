// Package iosiap 是 iOS IAP 适配器（v3 设计 §1.1/§1.2）：无服务端下单
// （CreatePayment=ErrUnsupported），ReceiptVerifier 走 verifyReceipt /
// StoreKit 2 JWS，回调走 App Store Server Notifications V2（JWS）。
// 退款不支持（引导用户找 Apple）。
package iosiap

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

const (
	defaultVerifyURL  = "https://buy.itunes.apple.com/verifyReceipt"
	defaultSandboxURL = "https://sandbox.itunes.apple.com/verifyReceipt"
	maxCallbackBody   = 1 << 20
	providerName      = payments.ProviderIOSIAP
	statusSandbox     = 21007
)

// Config 是 iOS IAP 适配器配置（secret 一律来自环境变量）。
type Config struct {
	BundleID         string
	SharedSecret     string
	AppleRootCert    string // PEM；空则使用内置 Apple Root CA G3
	VerifyReceiptURL string
	SandboxVerifyURL string
}

// Adapter 实现 PaymentProvider、ReceiptVerifier、CallbackAcker。
type Adapter struct {
	cfg   Config
	roots *x509.CertPool
	now   func() time.Time
	http  *http.Client
}

// New 构造适配器。未配置时构造成功，操作 fail-closed。
func New(cfg Config) *Adapter {
	if cfg.VerifyReceiptURL == "" {
		cfg.VerifyReceiptURL = defaultVerifyURL
	}
	if cfg.SandboxVerifyURL == "" {
		cfg.SandboxVerifyURL = defaultSandboxURL
	}
	pemData := cfg.AppleRootCert
	if strings.TrimSpace(pemData) == "" {
		pemData = appleRootCAG3PEM
	}
	roots, _ := certPoolFromPEM(pemData)
	return &Adapter{
		cfg:   cfg,
		roots: roots,
		now:   time.Now,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *Adapter) Name() string { return providerName }

func (a *Adapter) callbackConfigured() bool {
	return a.roots != nil
}

func (a *Adapter) receiptConfigured() bool {
	return a.cfg.SharedSecret != "" || a.roots != nil
}

// CreatePayment iOS 无服务端下单，走 VerifyReceipt。
func (a *Adapter) CreatePayment(context.Context, payments.CreatePaymentInput) (*payments.PaymentSession, error) {
	return nil, payments.ErrUnsupported
}

// Refund iOS IAP 不支持服务端退款（设计 §1.2：引导用户找 Apple）。
func (a *Adapter) Refund(context.Context, payments.RefundInput) (*payments.RefundResult, error) {
	return nil, payments.ErrUnsupported
}

// CallbackAck App Store 只需 HTTP 2xx；空 body。
func (a *Adapter) CallbackAck(success bool) (int, string, []byte) {
	if success {
		return http.StatusOK, "application/json", nil
	}
	return http.StatusInternalServerError, "application/json", nil
}

// VerifyCallback 验签 ASN V2 signedPayload JWS 并归一化。
func (a *Adapter) VerifyCallback(ctx context.Context, headers http.Header, rawBody []byte) (*payments.CallbackEvent, error) {
	if len(rawBody) > maxCallbackBody {
		return nil, payments.ErrSignatureInvalid
	}
	if !a.callbackConfigured() {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	var env asnEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil || env.SignedPayload == "" {
		return nil, payments.ErrSignatureInvalid
	}
	payload, err := parseAndVerifyJWS(env.SignedPayload, a.roots, a.now())
	if err != nil {
		return nil, payments.ErrSignatureInvalid
	}
	return a.normalizeNotification(payload, rawBody)
}

type asnEnvelope struct {
	SignedPayload string `json:"signedPayload"`
}

type asnNotification struct {
	NotificationType string `json:"notificationType"`
	Subtype          string `json:"subtype"`
	NotificationUUID string `json:"notificationUUID"`
	Data             struct {
		BundleID              string `json:"bundleId"`
		Environment           string `json:"environment"`
		SignedTransactionInfo string `json:"signedTransactionInfo"`
	} `json:"data"`
}

type signedTransaction struct {
	TransactionID         string `json:"transactionId"`
	OriginalTransactionID string `json:"originalTransactionId"`
	ProductID             string `json:"productId"`
	BundleID              string `json:"bundleId"`
	PurchaseDate          int64  `json:"purchaseDate"`
	Type                  string `json:"type"`
	Price                 int64  `json:"price"`
	Currency              string `json:"currency"`
	AppAccountToken       string `json:"appAccountToken"`
}

func (a *Adapter) normalizeNotification(payload, rawBody []byte) (*payments.CallbackEvent, error) {
	var n asnNotification
	if err := json.Unmarshal(payload, &n); err != nil {
		return nil, fmt.Errorf("iosiap: decode notification: %w", err)
	}
	if n.NotificationUUID == "" {
		return nil, fmt.Errorf("iosiap: notification missing uuid")
	}
	out := &payments.CallbackEvent{
		Provider:        providerName,
		ProviderEventID: n.NotificationUUID,
		Raw:             append([]byte(nil), rawBody...),
		ReceivedAt:      a.now(),
		Type:            "ignored",
	}
	if n.Data.SignedTransactionInfo != "" {
		txPayload, err := parseAndVerifyJWS(n.Data.SignedTransactionInfo, a.roots, a.now())
		if err != nil {
			return nil, payments.ErrSignatureInvalid
		}
		var tx signedTransaction
		if err := json.Unmarshal(txPayload, &tx); err != nil {
			return nil, fmt.Errorf("iosiap: decode transaction: %w", err)
		}
		if a.cfg.BundleID != "" && tx.BundleID != "" && tx.BundleID != a.cfg.BundleID {
			return nil, payments.ErrSignatureInvalid
		}
		out.ProviderOrderID = tx.TransactionID
		out.ProviderSessionID = tx.OriginalTransactionID
		out.OrderID = tx.AppAccountToken
		out.Amount = milliunitsToMinor(tx.Price, tx.Currency)
		out.Currency = strings.ToUpper(tx.Currency)
	}
	switch n.NotificationType {
	case "ONE_TIME_CHARGE":
		out.Type = payments.CallbackPaid
	case "REFUND", "REFUND_DECLINED":
		if n.NotificationType == "REFUND" {
			out.Type = payments.CallbackRefunded
		}
	case "EXPIRED", "REVOKE":
		out.Type = payments.CallbackFailed
	}
	return out, nil
}

// milliunitsToMinor 把 App Store price（货币 milliunits）转 ISO 最小单位。
func milliunitsToMinor(milli int64, currency string) int64 {
	if milli == 0 {
		return 0
	}
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "VND", "CLP", "ISK", "UGX", "XAF", "XOF", "XPF":
		return milli / 1000
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return milli
	default:
		return milli / 10
	}
}

// VerifyReceipt 校验 StoreKit 2 JWS 或 legacy verifyReceipt。
func (a *Adapter) VerifyReceipt(ctx context.Context, in payments.VerifyReceiptInput) (*payments.VerifiedPurchase, error) {
	if !a.receiptConfigured() {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	raw := strings.TrimSpace(string(in.Receipt))
	if looksLikeJWS(raw) {
		return a.verifyJWSTransaction(raw)
	}
	return a.verifyReceiptAPI(ctx, raw)
}

func (a *Adapter) verifyJWSTransaction(token string) (*payments.VerifiedPurchase, error) {
	if a.roots == nil {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	payload, err := parseAndVerifyJWS(token, a.roots, a.now())
	if err != nil {
		return nil, err
	}
	var tx signedTransaction
	if err := json.Unmarshal(payload, &tx); err != nil {
		return nil, fmt.Errorf("iosiap: decode signed transaction: %w", err)
	}
	if tx.TransactionID == "" {
		return nil, payments.ErrSignatureInvalid
	}
	if a.cfg.BundleID != "" && tx.BundleID != "" && tx.BundleID != a.cfg.BundleID {
		return nil, payments.ErrSignatureInvalid
	}
	paidAt := a.now()
	if tx.PurchaseDate > 0 {
		paidAt = time.UnixMilli(tx.PurchaseDate)
	}
	return &payments.VerifiedPurchase{
		TransactionID:         tx.TransactionID,
		OriginalTransactionID: tx.OriginalTransactionID,
		ProductID:             tx.ProductID,
		Amount:                milliunitsToMinor(tx.Price, tx.Currency),
		Currency:              strings.ToUpper(tx.Currency),
		PaidAt:                paidAt,
		BundleID:              tx.BundleID,
	}, nil
}

type verifyReceiptRequest struct {
	ReceiptData            string `json:"receipt-data"`
	Password               string `json:"password,omitempty"`
	ExcludeOldTransactions bool   `json:"exclude-old-transactions"`
}

type verifyReceiptResponse struct {
	Status            int             `json:"status"`
	Environment       string          `json:"environment"`
	Receipt           json.RawMessage `json:"receipt"`
	LatestReceiptInfo json.RawMessage `json:"latest_receipt_info"`
}

type receiptInApp struct {
	TransactionID         string `json:"transaction_id"`
	OriginalTransactionID string `json:"original_transaction_id"`
	ProductID             string `json:"product_id"`
	PurchaseDateMS        string `json:"purchase_date_ms"`
	BundleID              string `json:"bundle_id"`
}

func (a *Adapter) verifyReceiptAPI(ctx context.Context, receiptData string) (*payments.VerifiedPurchase, error) {
	if a.cfg.SharedSecret == "" {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	resp, err := a.postVerify(ctx, a.cfg.VerifyReceiptURL, receiptData)
	if err != nil {
		return nil, err
	}
	if resp.Status == statusSandbox {
		resp, err = a.postVerify(ctx, a.cfg.SandboxVerifyURL, receiptData)
		if err != nil {
			return nil, err
		}
	}
	if resp.Status != 0 {
		return nil, payments.ErrSignatureInvalid
	}
	item, err := pickLatestInApp(resp)
	if err != nil {
		return nil, err
	}
	if item.TransactionID == "" {
		return nil, payments.ErrSignatureInvalid
	}
	paidAt := a.now()
	if ms, err := strconv.ParseInt(item.PurchaseDateMS, 10, 64); err == nil && ms > 0 {
		paidAt = time.UnixMilli(ms)
	}
	env := resp.Environment
	return &payments.VerifiedPurchase{
		TransactionID:         item.TransactionID,
		OriginalTransactionID: item.OriginalTransactionID,
		ProductID:             item.ProductID,
		PaidAt:                paidAt,
		Environment:           env,
		BundleID:              item.BundleID,
	}, nil
}

func (a *Adapter) postVerify(ctx context.Context, endpoint, receiptData string) (*verifyReceiptResponse, error) {
	body, err := json.Marshal(verifyReceiptRequest{
		ReceiptData:            receiptData,
		Password:               a.cfg.SharedSecret,
		ExcludeOldTransactions: true,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("iosiap: verifyReceipt: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCallbackBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &payments.ProviderError{Provider: providerName, Status: resp.StatusCode}
	}
	var out verifyReceiptResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("iosiap: decode verifyReceipt: %w", err)
	}
	return &out, nil
}

func pickLatestInApp(resp *verifyReceiptResponse) (receiptInApp, error) {
	var items []receiptInApp
	if len(resp.LatestReceiptInfo) > 0 && string(resp.LatestReceiptInfo) != "null" {
		if err := json.Unmarshal(resp.LatestReceiptInfo, &items); err != nil {
			var one receiptInApp
			if err2 := json.Unmarshal(resp.LatestReceiptInfo, &one); err2 != nil {
				return receiptInApp{}, fmt.Errorf("iosiap: decode latest_receipt_info: %w", err)
			}
			items = []receiptInApp{one}
		}
	}
	if len(items) == 0 && len(resp.Receipt) > 0 {
		var wrap struct {
			BundleID string         `json:"bundle_id"`
			InApp    []receiptInApp `json:"in_app"`
		}
		if err := json.Unmarshal(resp.Receipt, &wrap); err == nil {
			for i := range wrap.InApp {
				if wrap.InApp[i].BundleID == "" {
					wrap.InApp[i].BundleID = wrap.BundleID
				}
			}
			items = wrap.InApp
		}
	}
	if len(items) == 0 {
		return receiptInApp{}, payments.ErrSignatureInvalid
	}
	best := items[0]
	bestMS, _ := strconv.ParseInt(best.PurchaseDateMS, 10, 64)
	for _, it := range items[1:] {
		ms, _ := strconv.ParseInt(it.PurchaseDateMS, 10, 64)
		if ms >= bestMS {
			best = it
			bestMS = ms
		}
	}
	return best, nil
}

var (
	_ payments.PaymentProvider = (*Adapter)(nil)
	_ payments.ReceiptVerifier = (*Adapter)(nil)
	_ payments.CallbackAcker   = (*Adapter)(nil)
)
