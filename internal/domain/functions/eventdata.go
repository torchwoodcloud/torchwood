// 事件触发器的入队 data 投影（functions v3 切片 D，docs/design/functions-v3.md
// §4.2/D12：data 只带投影「ID + 摘要」，全量靠 databases:read 回读——与 HTTP
// 触发器封套「小包透传 + 按需取」同一哲学）。
package functions

import (
	"encoding/json"

	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
)

// EventDataBudgetBytes 是事件触发器入队 data（TW_DATA）的字节预算：与执行
// data 上限 32KB 同源（app 层 maxExecutionDataBytes；领域层单列常量避免
// domain → app 反向依赖）。事件信封可达 1MiB，二者矛盾由两级截断化解：
//   - envelope_truncated：信封自身在 outbox 序列化期的 1MiB 预算截断
//     （= Envelope.Truncated，**先于**本投影发生）；
//   - data_truncated：文档投影塞不进剩余预算时剥掉 data（**后于**信封截断）。
//
// 两级分名是「对抗审查修正」明确要求（truncated 撞名）。
const EventDataBudgetBytes = 32 << 10

// EventInvocationData 是投递给函数的 data 投影（JSON object；字段清单与
// 设计 §4.2 逐字对应，document_id 是函数按 databases:read 回读全量的键）。
type EventInvocationData struct {
	Type         string `json:"type"` // 恒 "event"
	Event        string `json:"event"`
	EventID      string `json:"event_id"`
	Seq          int64  `json:"seq"`
	DatabaseID   string `json:"database_id"`
	CollectionID string `json:"collection_id"`
	DocumentID   string `json:"document_id"`
	Version      int64  `json:"version"`
	// EnvelopeTruncated = 信封级截断（outbox 1MiB 预算，Envelope.Truncated）。
	EnvelopeTruncated bool `json:"envelope_truncated"`
	// Data 是文档投影（与 REST Document 同形：id/data/permissions/…，
	// domainevents.DocumentPayload）；delete 事件与超限截断时缺省。
	Data map[string]any `json:"data,omitempty"`
	// DataTruncated = 投影级截断（文档投影超出 32KB 减固定字段的剩余预算，
	// data 被剥除——函数按 document_id 回读全量）。
	DataTruncated bool `json:"data_truncated,omitempty"`
}

// BuildEventInvocationData 由信封构建入队 data 投影（budget = 32KB 减固定
// 字段：先序列化固定字段，文档投影「尽力塞入」，整体超预算即剥 data 并标
// data_truncated——单次降级、不再尝试部分截断，保持投影 JSON 始终完整可
// 解析）。返回序列化后的 JSON object 字符串。
func BuildEventInvocationData(ev *domainevents.Envelope) (string, error) {
	out := EventInvocationData{
		Type:              "event",
		Event:             ev.Event,
		EventID:           ev.EventID,
		Seq:               ev.Seq,
		DatabaseID:        ev.DatabaseID,
		CollectionID:      ev.CollectionID,
		DocumentID:        ev.DocumentID,
		Version:           ev.Version,
		EnvelopeTruncated: ev.Truncated,
	}
	if ev.Data != nil {
		out.Data = domainevents.DocumentPayload(ev.Data)
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	if len(payload) <= EventDataBudgetBytes {
		return string(payload), nil
	}
	// 投影级截断：剥掉 data（两级截断分名——envelope_truncated 已独立承载
	// 信封级语义，见上）。
	out.Data = nil
	out.DataTruncated = true
	payload, err = json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}
