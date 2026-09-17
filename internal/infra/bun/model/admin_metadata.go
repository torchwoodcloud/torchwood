package model

import (
	"database/sql/driver"
	"encoding/json"
)

// AdminMetadata 是 admins.metadata JSONB 列的读取容忍投影。
//
// 该列语义为"通用偏好"（迁移 000010：后续新增偏好键免迁移，typed 字段
// 投影仅 timezone），落库值不保证恒为字符串——任何合法 JSON 都可能存在。
// Scan 因此永不因形态报错：非 object 形态与非字符串值一律跳过，只保留
// 字符串值键。GetAdmin 在认证热路径上（validator 每请求读 admins 行），
// 偏好读不出来最多功能回退（如时区跟随浏览器），不能打挂整个 console
// 认证——2026-09-17 线上事故：metadata 含非字符串 JSON 值 → Scan error
// → Internal "admin lookup failed"，该 admin 的全部会话持续 500。
type AdminMetadata map[string]string

// Scan 容忍一切形态：SQL NULL / json null / 非 object / 非字符串值 →
// 跳过，返回已收集的字符串键（可能为空 map），永不报错（理由见类型注释）。
func (m *AdminMetadata) Scan(src any) error {
	out := AdminMetadata{}
	switch v := src.(type) {
	case nil:
		// SQL NULL：列约束 NOT NULL，防御性归一为空 map。
	case []byte:
		collectStringValues(v, out)
	case string:
		collectStringValues([]byte(v), out)
	default:
		// 未知驱动形态：空 map，不报错。
	}
	*m = out
	return nil
}

// Value 序列化回 JSONB 文本；nil/空 map 落 "{}"（列 NOT NULL，空偏好
// 语义即空对象，避免写入 jsonb 'null' 形态）。
func (m AdminMetadata) Value() (driver.Value, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(map[string]string(m))
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func collectStringValues(raw []byte, out AdminMetadata) {
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return // 坏字节/非 object（数组、标量）：保持空，不报错
	}
	for k, v := range values {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
}
