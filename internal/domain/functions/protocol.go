package functions

// 本文件是 Functions v4 调用协议的单一事实源（docs/design/functions-v4.md
// §4，owner 拍板 D1 + D10 双轴增补）。torchwood（调用方/签名方）、Go/TS FDK
//（服务方/验签方）与日后 fleetly 原生托管面共用本契约；两面漂移由
// X-Tw-Protocol-Version 头 + 双仓协议契约测试共同约束。
//
// 协议要点（v4 §4，D10 判定准则「有没有调用方在等应用层结果」）：
//   - RPC 轴（JSON-RPC 2.0，有 id，等 result/error）：invoke 与
//     http.request（sync/async_ack 同形态）；id = execution id（回显）。
//   - 事件轴（CloudEvents 1.0 structured mode，通知，无应用层结果）：
//     cron.tick / 事件触发 / webhook 投递——2xx = 送达。
//   - 签名/身份走 header（传输与载荷分离），两轴共用同一签名方案。
//   - RPC 轴响应：送达成功恒 HTTP 200，函数成败在 result / error 对象；
//     非 2xx / 超时 / 验签协议不符 = 平台语义错误。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ProtocolVersion 是扩展协议版本（X-Tw-Protocol-Version 头值）：JSON-RPC 本体
// 由 "jsonrpc":"2.0" 自版本化，本头管 method 词表 / 头语义 / 签名方案的演进。
const ProtocolVersion = "1"

// Method 词表：调用语义路由（v4 §4.1，D10 双轴增补后 RPC 轴仅两 method；
// cron 与事件触发走事件轴 CloudEvents，见 CloudEvent/CloudEventTypeCronTick）。
const (
	// MethodInvoke 是 client/server 调用面（params = 函数载荷，原 TW_DATA）。
	MethodInvoke = "invoke"
	// MethodHTTPRequest 是 HTTP 触发器（params = {envelope, body_b64}；
	// result = HTTPResult——fetch 语义经 result 形状表达；sync 与
	// async_ack 同形态——响应模式是平台投递策略，不换协议，D10）。
	MethodHTTPRequest = "http.request"
)

// ——事件轴：CloudEvents 1.0 structured mode（v4 §4.2，D10）——
// 判定准则「有没有调用方在等应用层结果」：cron / 事件触发 / webhook 投递
// 均为通知（无应用层结果，2xx = 送达），与 RPC 轴分轨。structured mode 使
// 全部属性落在 body 内——整体处于签名保护圈（binary mode 的 ce-* header
// 不受签名保护，故不采用）。

// CloudEventsSpecVersion 是本平台产出的 specversion（只产 1.0）。
const CloudEventsSpecVersion = "1.0"

// CloudEventTypeCronTick 是 cron 触发的时间事件 type（data = CronTick 形状：
// {trigger_id, scheduled_for}）。document/system 事件的 type 直接用事件
// 目录名（documents.created / auth.users.created …，见 events catalog）。
const CloudEventTypeCronTick = "cron.tick"

// CloudEvent 是事件轴投递信封（structured mode JSON）。扩展属性用
// CloudEvents 扩展命名约定（小写字母数字）。
type CloudEvent struct {
	Specversion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Time            string          `json:"time,omitempty"`
	DataContentType string          `json:"datacontenttype,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
	// TwProject 是项目 id（扩展属性）。
	TwProject string `json:"twproject,omitempty"`
	// TwSeq 是 outbox 全局序（cron 自产事件无，零值）。
	TwSeq int64 `json:"twseq,omitempty"`
}

// Header 常量（HTTP 头大小写不敏感；与 FDK sdk/go/functions/serve.go 头族
// 对齐，新增 Signature / ProtocolVersion 两头，退役 Execution-Id /
// Trigger-Envelope / Timeout-Seconds——超时由调用方 ctx 承载）。
const (
	// HeaderSignature 是签名头：`t=<unix秒>,v1=<hex>[,v2=<hex>]`（Stripe
	// 风格；v2 是密钥轮换窗口内的旧钥签名，验签方任一命中即过）。
	HeaderSignature = "X-Tw-Signature"
	// HeaderExecutionToken 是 twx_ 执行身份（铸造/校验链路不变）。
	HeaderExecutionToken = "X-Tw-Execution-Token" // #nosec G101 -- 头名常量非凭证
	// HeaderSource / HeaderInvokingUserID / HeaderProjectID 是调用身份投影。
	HeaderSource         = "X-Tw-Source"
	HeaderInvokingUserID = "X-Tw-Invoking-User-Id"
	HeaderProjectID      = "X-Tw-Project-Id"
	// HeaderProtocolVersion 是扩展协议版本头。
	HeaderProtocolVersion = "X-Tw-Protocol-Version"
)

// MaxResultBodyBytes 是单次调用响应的读取上限（对齐 v3 maxInvokeResponseBytes
// 口径；64KB 级旧上限会截断 JSON 导致解析静默归零——取 1MiB 覆盖）。
const MaxResultBodyBytes = 1 << 20

// SignatureTimestampTolerance 是签名时间窗（防重放；v4 §4 拍板 5 分钟）。
const SignatureTimestampTolerance = 5 * time.Minute

// ——签名方案——

// SignaturePayload 返回参与签名的载荷串：`<timestamp>.<raw body>`。
// 签名与验签两侧必须经由本函数构造/复算，禁止手拼（口径漂移即验签全灭）。
func SignaturePayload(timestamp int64, body []byte) string {
	return strconv.FormatInt(timestamp, 10) + "." + string(body)
}

// ComputeSignature 返回 HMAC-SHA256(secret, payload) 的 hex 编码。
func ComputeSignature(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(SignaturePayload(timestamp, body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// BuildSignatureHeader 组装签名头。entries 有序参与（label → 签名 hex），
// v1 = 当前钥；轮换窗口内追加 v2 = 旧钥签名（FDK 持旧钥者过渡期可验）。
func BuildSignatureHeader(timestamp int64, entries [][2]string) string {
	parts := []string{"t=" + strconv.FormatInt(timestamp, 10)}
	for _, e := range entries {
		parts = append(parts, e[0]+"="+e[1])
	}
	return strings.Join(parts, ",")
}

// ParseSignatureHeader 解析签名头为时间戳与「label → 签名 hex」映射；
// 缺 t / 空映射 / 非法整数为协议错误。
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

// VerifySignatureHeader 是验签方（FDK / webhook 接收方）的完整校验：
// 解析头 → 时间窗内 → 任一 (secret, 任意 label 签名) 对命中即过。
// secrets 是验签方持有的密钥集（轮换窗口内新旧并存）。所有比较走
// hmac.Equal（常数时间）。
func VerifySignatureHeader(header string, body []byte, secrets []string, now time.Time) error {
	t, sigs, err := ParseSignatureHeader(header)
	if err != nil {
		return err
	}
	delta := now.Sub(time.Unix(t, 0))
	if delta < -SignatureTimestampTolerance || delta > SignatureTimestampTolerance {
		return fmt.Errorf("signature header: timestamp %d outside tolerance (±%s)", t, SignatureTimestampTolerance)
	}
	for _, secret := range secrets {
		want := ComputeSignature(secret, t, body)
		for label, got := range sigs {
			if hmac.Equal([]byte(want), []byte(got)) {
				_ = label // 命中即过；label 仅诊断用途
				return nil
			}
		}
	}
	return errors.New("signature header: no signature match")
}

// ——JSON-RPC 2.0 wire 类型——

// JSON-RPC 保留错误码（spec §5.1）。函数应用错误走 server error 区段
// （-32000..-32099 及自定义），torchwood 不消费其语义、只透传记录。
const (
	RPCCodeParseError     = -32700
	RPCCodeInvalidRequest = -32600
	RPCCodeMethodNotFound = -32601
	RPCCodeInvalidParams  = -32602
	RPCCodeInternalError  = -32603
	// RPCCodeServerRangeStart 是 server error 区段下界（函数应用错误缺省）。
	RPCCodeServerRangeStart = -32000
	// RPCCodeServerRangeEnd 是 server error 区段上界（保留）。
	RPCCodeServerRangeEnd = -32099
)

// RPCRequest 是 torchwood → endpoint 的请求 wire 形态。
// Params 载荷形状随 Method：invoke = 函数载荷；cron.tick = CronTick 形状；
// event.document = 事件投影；http.request = HTTPRequestParams。
type RPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	// ID 是请求关联（= execution id；服务方原样回显）。
	ID string `json:"id"`
}

// RPCError 是 JSON-RPC error 对象（应用层错误的一等承载）。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// RPCResponse 是 endpoint → torchwood 的响应 wire 形态。
// 恒 HTTP 200；Result 与 Error 二选一（Error 非空 = 函数应用层失败，
// 是合法结果、不映射为送达失败）。
type RPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      string    `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// HTTPResult 是 http.request method 的 result 形状（fetch 语义经 result
// 表达，v4 §4：自定义 status/headers/body 是一等结果，不映射执行失败）。
// BodyB64 是响应 body 的 base64；Headers 已滤 hop-by-hop（服务方职责）。
type HTTPResult struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	BodyB64 string            `json:"body_b64,omitempty"`
}

// TriggerEnvelope 是 HTTP 触发器封套元数据（http.request method 的 params
// 组成，v4 §4.1）：封套与原始 body 收编为单 body——旧「封套走
// x-tw-trigger-envelope 分发 header、body 走分发 body」双通道退役。Headers
// 与 TW_DATA 封套同一白名单（x-* + content-type + wechatpay-*，handler 构建
// 期剥除非白名单）。fetch 语义 FDK 据此还原 Request（url =
// http://trigger{path}?{raw_query}）。上限：整体（含 base64 body）随
// http.request params 落在调用 body 通道（4MB 级，handler 入口 413 判定）。
type TriggerEnvelope struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	RawQuery string              `json:"raw_query,omitempty"`
	Headers  map[string][]string `json:"headers,omitempty"`
}

// HTTPRequestParams 是 http.request method 的 params 形状：TriggerEnvelope
// 元数据 + 原始 body（base64）。对应旧分发面「封套走 header、body 走分发
// body」的双通道，收编为单 body。
type HTTPRequestParams struct {
	Envelope *TriggerEnvelope `json:"envelope"`
	BodyB64  string           `json:"body_b64,omitempty"`
}
