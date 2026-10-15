package functions

// v4 调用协议的验签面（docs/design/functions-v4.md §4）。SDK 是独立 module、
// 不能 import 平台 internal/——本文件自持签名方案实现，口径与
// internal/domain/functions/protocol.go **逐字节一致**：载荷串 `<t>.<body>`、
// HMAC-SHA256 hex、头格式 t=/v1=/v2=、时间窗 ±5 分钟、多密钥任一命中、
// 常数时间比较。两面漂移由共享基准向量测试钉死（canonicalSignatureVector）。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 协议头（与 serve.go 头族同处声明；v4 新增）。
const (
	hdrSignature       = "X-Tw-Signature"
	hdrProtocolVersion = "X-Tw-Protocol-Version"
)

// ProtocolVersion 是扩展协议版本（与平台 domain protocol.go 同值）。
const ProtocolVersion = "1"

// signatureTimestampTolerance 是签名时间窗（防重放）。
const signatureTimestampTolerance = 5 * time.Minute

// canonicalSignatureVector 是跨仓基准向量：HMAC-SHA256("whsec_vector",
// "1759000000.abc") 的 hex。平台侧 protocol_test.go 与本包测试共用本向量，
// 任一侧口径漂移即红。
const canonicalSignatureVector = "e91f630384f34686e4d97568d65a25531151d944aabd0569f4cfbe39282f29c6"

// SignaturePayload 返回参与签名的载荷串（口径：单一函数，禁止手拼）。
func SignaturePayload(timestamp int64, body []byte) string {
	return strconv.FormatInt(timestamp, 10) + "." + string(body)
}

// ComputeSignature 返回 HMAC-SHA256(secret, payload) 的 hex 编码。
func ComputeSignature(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(SignaturePayload(timestamp, body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// BuildSignatureHeader 组装签名头（entries：v1=当前钥[,v2=轮换旧钥]）。
// 本地开发联调（torchwood functions dev / 自签测试）用。
func BuildSignatureHeader(timestamp int64, entries [][2]string) string {
	parts := []string{"t=" + strconv.FormatInt(timestamp, 10)}
	for _, e := range entries {
		parts = append(parts, e[0]+"="+e[1])
	}
	return strings.Join(parts, ",")
}

// ParseSignatureHeader 解析签名头（缺 t / 空签名 / 非法时间戳 fail-closed）。
func ParseSignatureHeader(header string) (timestamp int64, sigs map[string]string, err error) {
	sigs = make(map[string]string)
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k == "" || v == "" {
			continue
		}
		if k == "t" {
			timestamp, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, nil, fmt.Errorf("signature header: invalid timestamp %q", v)
			}
			continue
		}
		sigs[k] = v
	}
	if timestamp == 0 {
		return 0, nil, errors.New("signature header: missing t")
	}
	if len(sigs) == 0 {
		return 0, nil, errors.New("signature header: no signature entries")
	}
	return timestamp, sigs, nil
}

// VerifySignatureHeader 是完整校验：时间窗内 + 任一 (secret, 签名) 命中
// （常数时间比较）。secrets 是函数侧持有的密钥集（轮换窗口内新旧并存）。
func VerifySignatureHeader(header string, body []byte, secrets []string, now time.Time) error {
	t, sigs, err := ParseSignatureHeader(header)
	if err != nil {
		return err
	}
	delta := now.Sub(time.Unix(t, 0))
	if delta < -signatureTimestampTolerance || delta > signatureTimestampTolerance {
		return fmt.Errorf("signature header: timestamp %d outside tolerance", t)
	}
	for _, secret := range secrets {
		want := ComputeSignature(secret, t, body)
		for _, got := range sigs {
			if hmac.Equal([]byte(want), []byte(got)) {
				return nil
			}
		}
	}
	return errors.New("signature header: no signature match")
}

// Verify 是验签中间件：校验 X-Tw-Signature（对原始 body 逐字节验签），
// 失败 401；成功后 body 原样续传给下一 handler。secrets 至少一个（空集
// fail-closed——无密钥等于不设防）。本地开发联调可经 VerifySkip 显式豁免。
func Verify(secrets ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(secrets) == 0 {
				http.Error(w, "functions: verify middleware requires at least one secret", http.StatusInternalServerError)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
			if err != nil {
				http.Error(w, "functions: read body: "+err.Error(), http.StatusBadRequest)
				return
			}
			if len(body) > maxBodyBytes {
				http.Error(w, "functions: body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if err := VerifySignatureHeader(r.Header.Get(hdrSignature), body, secrets, time.Now()); err != nil {
				http.Error(w, "functions: "+err.Error(), http.StatusUnauthorized)
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			next.ServeHTTP(w, r)
		})
	}
}
