package crud

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

// 时间 keyset 游标语义单点测试：方向化格式（'a'/'d' 前缀 + RFC3339Nano，
// base64url 无填充）、可选 \x1f 决胜段、legacy 无前缀 = DESC、方向闸。
// 全仓时间游标（server/client gRPC 列表与 billing 查询）共用本实现。

func timeCursorRoundTrip(t *testing.T, c TimeCursor) TimeCursor {
	t.Helper()
	got, err := DecodeTimeCursor(EncodeTimeCursor(c))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestTimeCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 9, 22, 10, 30, 0, 123456789, time.UTC)

	for _, tc := range []struct {
		name string
		c    TimeCursor
	}{
		{"desc", TimeCursor{Time: ts}},
		{"asc", TimeCursor{Ascending: true, Time: ts}},
		{"desc-tiebreak", TimeCursor{Time: ts, Tiebreak: "01JC3R9Z7GAXJ5K2Q8WY0P4M6D"}},
		{"asc-tiebreak", TimeCursor{Ascending: true, Time: ts, Tiebreak: "rollup-id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := timeCursorRoundTrip(t, tc.c)
			if !got.Time.Equal(tc.c.Time) {
				t.Fatalf("time = %v, want %v", got.Time, tc.c.Time)
			}
			if got.Ascending != tc.c.Ascending {
				t.Fatalf("ascending = %v, want %v", got.Ascending, tc.c.Ascending)
			}
			if got.Tiebreak != tc.c.Tiebreak {
				t.Fatalf("tiebreak = %q, want %q", got.Tiebreak, tc.c.Tiebreak)
			}
		})
	}
}

// 非 UTC 时区归一为 UTC 编码（往返后时刻相等）。
func TestTimeCursorUTCNormalized(t *testing.T) {
	ts := time.Date(2026, 9, 22, 18, 30, 0, 0, time.FixedZone("CST", 8*3600))
	got := timeCursorRoundTrip(t, TimeCursor{Time: ts})
	if !got.Time.Equal(ts) {
		t.Fatalf("time = %v, want %v", got.Time, ts)
	}
}

// 旧格式：无方向前缀 = DESC（存量 token 兼容），含带 \x1f 决胜段的旧版。
func TestDecodeTimeCursorLegacyToken(t *testing.T) {
	ts := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	legacy := base64.RawURLEncoding.EncodeToString([]byte(ts.Format(time.RFC3339Nano)))
	c, err := DecodeTimeCursor(legacy)
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if !c.Time.Equal(ts) || c.Ascending || c.Tiebreak != "" {
		t.Fatalf("got %+v, want (ts, false, \"\")", c)
	}

	legacyTiebreak := base64.RawURLEncoding.EncodeToString([]byte(ts.Format(time.RFC3339Nano) + "\x1f" + "id-1"))
	c, err = DecodeTimeCursor(legacyTiebreak)
	if err != nil {
		t.Fatalf("decode legacy tiebreak: %v", err)
	}
	if !c.Time.Equal(ts) || c.Ascending || c.Tiebreak != "id-1" {
		t.Fatalf("got %+v, want (ts, false, \"id-1\")", c)
	}
}

func TestDecodeTimeCursorInvalid(t *testing.T) {
	if _, err := DecodeTimeCursor("not-base64!!"); err == nil {
		t.Fatal("expected error for non-base64 token")
	}
	// 带前缀但时间段非法。
	bad := base64.RawURLEncoding.EncodeToString([]byte("a: not-a-time"))
	if _, err := DecodeTimeCursor(bad); err == nil {
		t.Fatal("expected error for malformed time")
	}
	// 无前缀但时间段非法。
	badLegacy := base64.RawURLEncoding.EncodeToString([]byte("not-a-time"))
	if _, err := DecodeTimeCursor(badLegacy); err == nil {
		t.Fatal("expected error for malformed legacy time")
	}
	// 空 token = 第一页，零值直接放行。
	c, err := DecodeTimeCursor("")
	if err != nil || !c.Time.IsZero() {
		t.Fatalf("empty token: got (%+v, %v)", c, err)
	}
}

func TestDecodeTimeCursorDirectionGate(t *testing.T) {
	ts := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)

	// 方向一致放行。
	if _, err := DecodeTimeCursorDirection(EncodeTimeCursor(TimeCursor{Time: ts, Ascending: true}), true); err != nil {
		t.Fatalf("matching asc: %v", err)
	}
	// 方向不一致拒绝（换向必须从第一页重来）。
	if _, err := DecodeTimeCursorDirection(EncodeTimeCursor(TimeCursor{Time: ts, Ascending: true}), false); !errors.Is(err, ErrCursorDirection) {
		t.Fatalf("mismatched cursor: err = %v, want ErrCursorDirection", err)
	}
	// 旧格式 token = DESC：DESC 请求放行，ASC 请求拒绝。
	legacy := base64.RawURLEncoding.EncodeToString([]byte(ts.Format(time.RFC3339Nano)))
	if _, err := DecodeTimeCursorDirection(legacy, false); err != nil {
		t.Fatalf("legacy token under desc: %v", err)
	}
	if _, err := DecodeTimeCursorDirection(legacy, true); !errors.Is(err, ErrCursorDirection) {
		t.Fatalf("legacy token under asc: err = %v, want ErrCursorDirection", err)
	}
	// 空 token（第一页）任何方向都放行。
	if _, err := DecodeTimeCursorDirection("", true); err != nil {
		t.Fatalf("empty token: %v", err)
	}
}
