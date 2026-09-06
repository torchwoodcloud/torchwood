package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// DocumentEventsOutbox 是用户集合文档写事件的 transactional outbox 行
// （public 元数据库，与文档写同 COMMIT；payload 为完整事件信封 JSON，含 acl）。
// v3 起经济事件复用同表（设计 §5.1）：channel 落显式扇出频道（D17），
// 文档事件 channel 为 NULL。
// seq 为全局分配序（阶段④，000002 GENERATED ALWAYS AS IDENTITY）：INSERT
// 必须列白名单排除本列（bun 无 identity 特判），读取走 SELECT 列清单。
type DocumentEventsOutbox struct {
	bun.BaseModel `bun:"table:document_events_outbox,alias:deo"`

	EventID      string          `bun:"event_id,pk"`
	Seq          int64           `bun:"seq,notnull"`
	ProjectID    string          `bun:"project_id,notnull"`
	Topic        string          `bun:"topic,notnull"`
	Channel      *string         `bun:"channel"`
	Payload      json.RawMessage `bun:"payload,type:jsonb,notnull"`
	CreatedAt    time.Time       `bun:"created_at,notnull"`
	AvailableAt  time.Time       `bun:"available_at,notnull"`
	Attempts     int             `bun:"attempts,notnull,default:0"`
	DispatchedAt *time.Time      `bun:"dispatched_at"`
	PublishedAt  *time.Time      `bun:"published_at"`
}
