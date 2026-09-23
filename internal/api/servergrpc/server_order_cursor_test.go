package servergrpc

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

// 时间 keyset 游标的方向化格式：'a'/'d' 前缀 + RFC3339Nano（base64url）；
// 无前缀的旧格式 = DESC（存量 token 兼容）。
func TestServerOrderCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 9, 22, 10, 30, 0, 123456789, time.UTC)

	for _, tc := range []struct {
		name      string
		ascending bool
	}{
		{"desc", false},
		{"asc", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := encodeServerOrderCursor(ts, tc.ascending)
			got, ascending, err := decodeServerOrderCursor(token)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !got.Equal(ts) {
				t.Fatalf("time = %v, want %v", got, ts)
			}
			if ascending != tc.ascending {
				t.Fatalf("ascending = %v, want %v", ascending, tc.ascending)
			}
		})
	}
}

func TestDecodeServerOrderCursorLegacyToken(t *testing.T) {
	ts := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	// 旧格式：无方向前缀，语义 = DESC。
	legacy := base64.RawURLEncoding.EncodeToString([]byte(ts.UTC().Format(time.RFC3339Nano)))
	got, ascending, err := decodeServerOrderCursor(legacy)
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if !got.Equal(ts) || ascending {
		t.Fatalf("got (%v, %v), want (%v, false)", got, ascending, ts)
	}
}

func TestDecodeServerOrderCursorInvalid(t *testing.T) {
	if _, _, err := decodeServerOrderCursor("not-base64!!"); err == nil {
		t.Fatal("expected error for non-base64 token")
	}
	// 带前缀但时间段非法。
	bad := base64.RawURLEncoding.EncodeToString([]byte("a: not-a-time"))
	if _, _, err := decodeServerOrderCursor(bad); err == nil {
		t.Fatal("expected error for malformed time")
	}
}

func TestDecodeServerOrderPageDirectionGate(t *testing.T) {
	ts := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)

	// 方向一致放行。
	if _, err := decodeServerOrderPage(encodeServerOrderCursor(ts, true), true); err != nil {
		t.Fatalf("matching asc: %v", err)
	}
	// 方向不一致拒绝（换向必须从第一页重来）。
	if _, err := decodeServerOrderPage(encodeServerOrderCursor(ts, true), false); !errors.Is(err, errServerOrderMismatch) {
		t.Fatalf("mismatched cursor: err = %v, want errServerOrderMismatch", err)
	}
	// 旧格式 token = DESC：DESC 请求放行，ASC 请求拒绝。
	legacy := base64.RawURLEncoding.EncodeToString([]byte(ts.Format(time.RFC3339Nano)))
	if _, err := decodeServerOrderPage(legacy, false); err != nil {
		t.Fatalf("legacy token under desc: %v", err)
	}
	if _, err := decodeServerOrderPage(legacy, true); !errors.Is(err, errServerOrderMismatch) {
		t.Fatalf("legacy token under asc: err = %v, want errServerOrderMismatch", err)
	}
	// 空 token（第一页）任何方向都放行。
	if _, err := decodeServerOrderPage("", true); err != nil {
		t.Fatalf("empty token: %v", err)
	}
}
