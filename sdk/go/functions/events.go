package functions

import (
	"encoding/json"
	"fmt"
	"time"
)

// CronTick 是 cron 触发的一次心跳（TW_DATA {type:"cron",trigger_id,
// scheduled_for} 的 typed 投影；scheduled_for 为计划时刻 RFC3339，函数内
// 可作幂等键）。
type CronTick struct {
	TriggerID    string    `json:"trigger_id"`
	ScheduledFor time.Time `json:"scheduled_for"`
}

// DocumentChange 是数据库事件触发的一次变更（TW_DATA 事件投影的 typed
// 视图；投影只带「ID + 摘要」，全量靠 Client.GetDocument 回读）。
type DocumentChange struct {
	// Event 是事件名（如 documents.created / documents.deleted）。
	Event string `json:"event"`
	// DatabaseID / CollectionID / DocumentID 定位变更文档（回读全量的键）。
	DatabaseID   string `json:"database_id"`
	CollectionID string `json:"collection_id"`
	DocumentID   string `json:"document_id"`
	// Version 是文档 OCC 版本（事件时刻）。
	Version int64 `json:"version"`
	// Document 是文档投影（delete 事件与超限截断时为零值——用 DocumentID
	// 回读判断；SDK 不重复暴露 event_id/seq/truncated 等原始字段，需要时经
	// Mux.Invoke 消费原始 TW_DATA）。
	Document Document `json:"document"`
}

// Document 是文档投影（REST Document 的 {id,data} 子集）。
type Document struct {
	ID   string         `json:"id"`
	Data map[string]any `json:"data,omitempty"`
}

// parseCronTick 解析 cron TW_DATA（JSON 语法错误已由 twData → 400；此处
// 仅捕获形状不匹配 → error → 500 封套）。
func parseCronTick(data json.RawMessage) (CronTick, error) {
	var wire struct {
		TriggerID    string `json:"trigger_id"`
		ScheduledFor string `json:"scheduled_for"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return CronTick{}, fmt.Errorf("cron TW_DATA shape mismatch: %w", err)
	}
	if wire.TriggerID == "" {
		return CronTick{}, fmt.Errorf("cron TW_DATA: missing trigger_id")
	}
	at, err := time.Parse(time.RFC3339, wire.ScheduledFor)
	if err != nil {
		return CronTick{}, fmt.Errorf("cron TW_DATA: invalid scheduled_for %q: %v", wire.ScheduledFor, err)
	}
	return CronTick{TriggerID: wire.TriggerID, ScheduledFor: at}, nil
}

// parseDocumentChange 解析事件 TW_DATA（投影形状对照平台
// EventInvocationData：{type,event,event_id,seq,database_id,collection_id,
// document_id,version,data:{id,data,...},envelope_truncated,data_truncated}
// ——SDK 只取 typed 视图所需字段；data 缺省（delete/截断）→ Document 零值）。
func parseDocumentChange(data json.RawMessage) (DocumentChange, error) {
	var wire struct {
		Event        string `json:"event"`
		DatabaseID   string `json:"database_id"`
		CollectionID string `json:"collection_id"`
		DocumentID   string `json:"document_id"`
		Version      int64  `json:"version"`
		Data         *struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return DocumentChange{}, fmt.Errorf("event TW_DATA shape mismatch: %w", err)
	}
	switch {
	case wire.Event == "":
		return DocumentChange{}, fmt.Errorf("event TW_DATA: missing event")
	case wire.DatabaseID == "":
		return DocumentChange{}, fmt.Errorf("event TW_DATA: missing database_id")
	case wire.CollectionID == "":
		return DocumentChange{}, fmt.Errorf("event TW_DATA: missing collection_id")
	case wire.DocumentID == "":
		return DocumentChange{}, fmt.Errorf("event TW_DATA: missing document_id")
	}
	ch := DocumentChange{
		Event:        wire.Event,
		DatabaseID:   wire.DatabaseID,
		CollectionID: wire.CollectionID,
		DocumentID:   wire.DocumentID,
		Version:      wire.Version,
	}
	if wire.Data != nil {
		ch.Document = Document{ID: wire.Data.ID, Data: wire.Data.Data}
	}
	return ch, nil
}
