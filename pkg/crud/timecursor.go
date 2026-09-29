package crud

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// TimeCursor 是时间 keyset 游标（列表固定按时间列排序的方言）：不透明 token
// = base64url(方向前缀 + RFC3339Nano [+ \x1f + 决胜 id])。
//
//   - 方向前缀 "a:"=ASC / "d:"=DESC：方向决定游标语义（「晚于」vs「早于」），
//     跨方向复用 token 会静默错乱，DecodeTimeCursorDirection 在解码期拒绝；
//   - 无前缀旧格式 = DESC（存量 token 兼容：旧端点不带 sort_order，语义即
//     DESC）——旧端点签发的无前缀 token 与带 \x1f 决胜的旧格式均可解码；
//   - Tiebreak 是同刻多行的决胜字段（如 period_start 相同的账期行），进
//     keyset 谓词保证不重不漏。
//
// 全仓时间游标（server/client gRPC 列表与 billing 查询）统一走本实现；
// 方向闸语义与哨兵错误在本处单点测试。
type TimeCursor struct {
	Ascending bool
	Time      time.Time
	Tiebreak  string
}

// ErrCursorDirection 标记游标方向与请求排序方向不一致（换向必须从第一页
// 重新开始；携带异向游标 = 客户端 bug 或过期缓存，fail-fast 而非静默错位）。
// 调用方以 errors.Is 命中后映射 InvalidArgument。
var ErrCursorDirection = errors.New("page token sort order mismatch")

// EncodeTimeCursor 编码不透明游标（t 归一 UTC；Tiebreak 为空时省略决胜段）。
func EncodeTimeCursor(c TimeCursor) string {
	prefix := "d:"
	if c.Ascending {
		prefix = "a:"
	}
	s := prefix + c.Time.UTC().Format(time.RFC3339Nano)
	if c.Tiebreak != "" {
		s += "\x1f" + c.Tiebreak
	}
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// DecodeTimeCursor 解码游标；无前缀旧格式 = DESC（存量 token 兼容）。
// 非 base64 或非法时间返回 error；空 token 返回零值（第一页，方向不参与校验）。
func DecodeTimeCursor(token string) (TimeCursor, error) {
	if token == "" {
		return TimeCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return TimeCursor{}, err
	}
	s := string(raw)
	ascending := false
	if len(s) > 1 && (s[0] == 'a' || s[0] == 'd') && s[1] == ':' {
		ascending = s[0] == 'a'
		s = s[2:]
	}
	t, tiebreak, err := parseTimeCursorValue(s)
	if err != nil {
		return TimeCursor{}, err
	}
	return TimeCursor{Ascending: ascending, Time: t, Tiebreak: tiebreak}, nil
}

// DecodeTimeCursorDirection 解码并校验方向一致性。空 token（第一页）方向由
// 请求决定，直接放行；方向不一致返回 ErrCursorDirection。
func DecodeTimeCursorDirection(token string, ascending bool) (TimeCursor, error) {
	c, err := DecodeTimeCursor(token)
	if err != nil {
		return TimeCursor{}, err
	}
	if token != "" && c.Ascending != ascending {
		return TimeCursor{}, ErrCursorDirection
	}
	return c, nil
}

// parseTimeCursorValue 解析前缀后的载荷：RFC3339Nano [+ \x1f + 决胜 id]。
func parseTimeCursorValue(s string) (time.Time, string, error) {
	if i := strings.IndexByte(s, '\x1f'); i >= 0 {
		t, err := time.Parse(time.RFC3339Nano, s[:i])
		if err != nil {
			return time.Time{}, "", err
		}
		return t, s[i+1:], nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, "", err
	}
	return t, "", nil
}
