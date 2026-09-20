// Package shared 收录 server 面与 client 面 gRPC 传输层共用的文档映射辅助。
// 两面此前各自持有逐字重复的 docTimeField 实现，且都只认 string 形态——
// app 层 membershipAsDocument 写入的是 time.Time，导致 membership 的
// invited_at/joined_at 契约字段恒空（S11 修复：双形态兼容，两面共用一份）。
package shared

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// DocTimeField 从文档 data 取时间字段并转为 Timestamp。兼容两种存储形态：
//   - time.Time：app 层装配（如 membershipAsDocument）直接写入的原生时间值；
//   - string：文档面经 JSON 往返后的 RFC3339Nano 文本。
//
// 缺失、空串与不可解析值一律返回 nil（proto optional 字段缺省，不猜 0 值）。
func DocTimeField(data map[string]any, key string) *timestamppb.Timestamp {
	switch v := data[key].(type) {
	case time.Time:
		if v.IsZero() {
			return nil
		}
		return timestamppb.New(v)
	case string:
		if v == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil
		}
		return timestamppb.New(t)
	default:
		return nil
	}
}
