package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainevents "github.com/torchwooddev/torchwood/internal/domain/events"
)

// MarshalEnvelope 是 marshalEnvelope 的导出包装：Redis Stream 条目与
// outbox.payload 共用同一份序列化（含 acl 与 256 KiB 截断逻辑），保证
// worker XADD 的载荷与落库 payload 同形。
func MarshalEnvelope(ev domainevents.Envelope) (json.RawMessage, error) {
	return marshalEnvelope(ev)
}

// UnmarshalEnvelope 解码完整信封 JSON（outbox.payload / Stream 条目同形，
// 必须含 acl）。data / acl 缺省或为 null 时对应字段保持零值，不报错；
// permission 字符串按 "type:role" 字面量分段（不做 write 展开），保证
// Envelope → JSON → Envelope 无损往返。
func UnmarshalEnvelope(data []byte) (domainevents.Envelope, error) {
	var raw struct {
		EventID       string          `json:"event_id"`
		Event         string          `json:"event"`
		ProjectID     string          `json:"project_id"`
		DatabaseID    string          `json:"database_id"`
		CollectionID  string          `json:"collection_id"`
		DocumentID    string          `json:"document_id"`
		Version       int64           `json:"version"`
		Seq           int64           `json:"seq"`
		TransactionID string          `json:"transaction_id"`
		CreatedAt     string          `json:"created_at"`
		Truncated     bool            `json:"truncated"`
		Data          json.RawMessage `json:"data"`
		ACL           json.RawMessage `json:"acl"`
		Domain        string          `json:"domain"`
		Channel       string          `json:"channel"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return domainevents.Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	ev := domainevents.Envelope{
		EventID:       raw.EventID,
		Event:         raw.Event,
		ProjectID:     raw.ProjectID,
		DatabaseID:    raw.DatabaseID,
		CollectionID:  raw.CollectionID,
		DocumentID:    raw.DocumentID,
		Version:       raw.Version,
		Seq:           raw.Seq,
		TransactionID: raw.TransactionID,
		Truncated:     raw.Truncated,
		Domain:        raw.Domain,
		Channel:       raw.Channel,
	}
	if raw.CreatedAt != "" {
		createdAt, err := time.Parse(time.RFC3339, raw.CreatedAt)
		if err != nil {
			return domainevents.Envelope{}, fmt.Errorf("decode envelope created_at: %w", err)
		}
		ev.CreatedAt = createdAt
	}
	// 经济事件：Attrs = ClientPayload 去掉固定键（domain 非空时无 data/acl）。
	if ev.IsEconomy() {
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return domainevents.Envelope{}, fmt.Errorf("decode economy envelope: %w", err)
		}
		delete(m, "event_id")
		delete(m, "event")
		delete(m, "project_id")
		delete(m, "domain")
		delete(m, "channel")
		delete(m, "created_at")
		delete(m, "seq")
		delete(m, "transaction_id")
		ev.Attrs = m
		return ev, nil
	}
	if len(raw.Data) > 0 && string(raw.Data) != "null" {
		doc, err := unmarshalDocumentPayload(raw.Data)
		if err != nil {
			return domainevents.Envelope{}, err
		}
		ev.Data = &doc
	}
	if len(raw.ACL) > 0 && string(raw.ACL) != "null" {
		if err := unmarshalACLSnapshot(raw.ACL, &ev.ACL); err != nil {
			return domainevents.Envelope{}, err
		}
	}
	return ev, nil
}

// unmarshalDocumentPayload 解析与 REST Document 同形的 data 对象
// （DocumentPayload 输出）。
func unmarshalDocumentPayload(data []byte) (databases.Document, error) {
	var p struct {
		ID          string         `json:"id"`
		Data        map[string]any `json:"data"`
		Permissions []string       `json:"permissions"`
		CreatedAt   string         `json:"created_at"`
		UpdatedAt   string         `json:"updated_at"`
		Version     int64          `json:"version"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return databases.Document{}, fmt.Errorf("decode envelope data: %w", err)
	}
	doc := databases.Document{ID: p.ID, Data: p.Data, Version: p.Version}
	for _, ps := range p.Permissions {
		typ, role, ok := cutPermission(ps)
		if !ok {
			continue
		}
		doc.Permissions = append(doc.Permissions, databases.Permission{Type: typ, Role: role})
	}
	var err error
	if p.CreatedAt != "" {
		if doc.CreatedAt, err = time.Parse(time.RFC3339, p.CreatedAt); err != nil {
			return databases.Document{}, fmt.Errorf("decode envelope data created_at: %w", err)
		}
	}
	if p.UpdatedAt != "" {
		if doc.UpdatedAt, err = time.Parse(time.RFC3339, p.UpdatedAt); err != nil {
			return databases.Document{}, fmt.Errorf("decode envelope data updated_at: %w", err)
		}
	}
	return doc, nil
}

func unmarshalACLSnapshot(data []byte, acl *domainevents.ACLSnapshot) error {
	var a struct {
		DocumentSecurity      bool     `json:"document_security"`
		CollectionPermissions []string `json:"collection_permissions"`
		DocumentPermissions   []string `json:"document_permissions"`
		DocHasPerms           bool     `json:"doc_has_perms"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("decode envelope acl: %w", err)
	}
	acl.DocumentSecurity = a.DocumentSecurity
	acl.DocHasPerms = a.DocHasPerms
	acl.CollectionPermissions = parsePermissionStringsLiteral(a.CollectionPermissions)
	acl.DocumentPermissions = parsePermissionStringsLiteral(a.DocumentPermissions)
	return nil
}

// parsePermissionStringsLiteral 按 "type:role" 字面量分段（与
// databases.ParsePermissionStrings 不同：后者会把 write 展开为
// create/update/delete，破坏 Envelope ↔ JSON 往返）。存库的集合权限
// 都是具体类型，write 展开只存在于输入侧，不落在信封里。
func parsePermissionStringsLiteral(items []string) []databases.Permission {
	out := make([]databases.Permission, 0, len(items))
	for _, item := range items {
		typ, role, ok := cutPermission(item)
		if !ok {
			continue
		}
		out = append(out, databases.Permission{Type: typ, Role: role})
	}
	return out
}

func cutPermission(s string) (typ, role string, ok bool) {
	typ, role, ok = strings.Cut(strings.TrimSpace(s), ":")
	if !ok || typ == "" || role == "" {
		return "", "", false
	}
	return typ, role, true
}
