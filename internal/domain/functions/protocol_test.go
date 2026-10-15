package functions

import (
	"strings"
	"testing"
	"time"
)

// TestSignatureRoundTrip 覆盖签名方案全链：构造头 → 解析 → 验签命中；
// 篡改 body / 过期时间戳 / 错误密钥必须拒。
func TestSignatureRoundTrip(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"jsonrpc":"2.0","method":"invoke","params":{"a":1},"id":"exec-1"}`)
	now := time.Unix(1759000000, 0)

	header := BuildSignatureHeader(now.Unix(), [][2]string{
		{"v1", ComputeSignature(secret, now.Unix(), body)},
	})

	gotT, sigs, err := ParseSignatureHeader(header)
	if err != nil {
		t.Fatalf("ParseSignatureHeader: %v", err)
	}
	if gotT != now.Unix() {
		t.Fatalf("timestamp = %d, want %d", gotT, now.Unix())
	}
	if len(sigs) != 1 || sigs["v1"] == "" {
		t.Fatalf("sigs = %v, want single v1 entry", sigs)
	}

	if err := VerifySignatureHeader(header, body, []string{secret}, now); err != nil {
		t.Fatalf("VerifySignatureHeader: %v", err)
	}

	// 篡改 body 必须拒。
	if err := VerifySignatureHeader(header, append(body, '!'), []string{secret}, now); err == nil {
		t.Fatal("tampered body: want reject, got nil")
	}
	// 错误密钥必须拒。
	if err := VerifySignatureHeader(header, body, []string{"whsec_other"}, now); err == nil {
		t.Fatal("wrong secret: want reject, got nil")
	}
	// 时间窗过期必须拒（±5 分钟）。
	if err := VerifySignatureHeader(header, body, []string{secret}, now.Add(6*time.Minute)); err == nil {
		t.Fatal("expired timestamp: want reject, got nil")
	}
	if err := VerifySignatureHeader(header, body, []string{secret}, now.Add(-6*time.Minute)); err == nil {
		t.Fatal("future timestamp: want reject, got nil")
	}
}

// TestSignatureRotationWindow 轮换窗口：头携带 v1/v2 双签名，持旧钥的
// 验签方（secrets 只含旧钥）仍可命中。
func TestSignatureRotationWindow(t *testing.T) {
	newSecret, oldSecret := "whsec_new", "whsec_old"
	body := []byte("payload")
	ts := time.Unix(1759000100, 0)

	header := BuildSignatureHeader(ts.Unix(), [][2]string{
		{"v1", ComputeSignature(newSecret, ts.Unix(), body)},
		{"v2", ComputeSignature(oldSecret, ts.Unix(), body)},
	})

	if err := VerifySignatureHeader(header, body, []string{newSecret}, ts); err != nil {
		t.Fatalf("new-secret holder: %v", err)
	}
	if err := VerifySignatureHeader(header, body, []string{oldSecret}, ts); err != nil {
		t.Fatalf("old-secret holder (rotation window): %v", err)
	}
}

// TestParseSignatureHeaderErrors 缺 t / 非法时间戳 / 空签名为协议错误。
func TestParseSignatureHeaderErrors(t *testing.T) {
	cases := []string{
		"v1=deadbeef",       // 缺 t
		"t=abc,v1=deadbeef", // 非法时间戳
		"t=1759000000",      // 无签名项
		"",                  // 空头
	}
	for _, header := range cases {
		if _, _, err := ParseSignatureHeader(header); err == nil {
			t.Errorf("ParseSignatureHeader(%q) = nil error, want protocol error", header)
		}
	}
}

// TestSignaturePayloadShape 钉死签名载荷口径：`<t>.<body>`——两面（签名/
// 验签）与 FDK 复算必须逐字节一致，此测试即契约。
func TestSignaturePayloadShape(t *testing.T) {
	if got := SignaturePayload(1759000000, []byte("abc")); got != "1759000000.abc" {
		t.Fatalf("SignaturePayload = %q, want %q", got, "1759000000.abc")
	}
	if got := ComputeSignature("k", 1, []byte("b")); len(got) != 64 || !isHex(got) {
		t.Fatalf("ComputeSignature = %q, want 64-char hex", got)
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// TestRPCWireTypes 钉 wire 形态：RPC 轴 method 词表（D10 后仅两 method）、
// 事件轴 CloudEvents 常量、error 对象字段与协议版本。
func TestRPCWireTypes(t *testing.T) {
	if MethodInvoke != "invoke" || MethodHTTPRequest != "http.request" {
		t.Fatal("rpc method vocabulary drifted")
	}
	if CloudEventsSpecVersion != "1.0" || CloudEventTypeCronTick != "cron.tick" {
		t.Fatal("cloud events vocabulary drifted")
	}
	if ProtocolVersion != "1" {
		t.Fatal("protocol version drifted")
	}
	err := &RPCError{Code: RPCCodeServerRangeStart, Message: "boom"}
	if !strings.Contains(err.Error(), "-32000") {
		t.Fatalf("RPCError.Error() = %q, want code inside", err.Error())
	}
}
