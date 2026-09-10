// 事件订阅串的解析、校验与匹配（functions v3 切片 D，docs/design/functions-v3.md
// §4.1/D12）。订阅串格式对齐 Appwrite（业界同构、迁移友好）：
//
//	databases.{database_id}.collections.{collection_id}.documents.{op}
//
// 一期通配语义（设计 §4.1）：op 精确三事件（create/update/delete）或 `*`；
// collection 段精确或 `*`（设计给出的规范通配形态 `collections.*.documents.*`
// 是本语法的特例）；database 段一期必须精确（database 级通配后置）。
package functions

import (
	"fmt"
	"strings"
)

// 文档写事件 op 词表（internal/domain/events EventDocuments* 同源）。
const (
	EventOpCreate = "create"
	EventOpUpdate = "update"
	EventOpDelete = "delete"
	// EventSegmentAny 是通配段字面量（collection / op 段；database 段一期
	// 不允许，见 ParseEventPattern）。
	EventSegmentAny = "*"

	// MaxEventPatterns 是单个触发器允许的订阅串上限（protovalidate
	// max_items 同源；防止单条 config JSONB 无界膨胀）。
	MaxEventPatterns = 64
	// MaxEventPatternBytes 是单条订阅串的字节上限（protovalidate 同源；
	// 6 段 + 合法字符集下远达不到，仅防滥用）。
	MaxEventPatternBytes = 200
)

// EventPattern 是一条解析后的订阅串（通配段以 `*` 表示）。
type EventPattern struct {
	DatabaseID   string // 精确（一期不允许 `*`）
	CollectionID string // 精确 或 "*"
	Op           string // create|update|delete 或 "*"
}

// Matches 报告该订阅串是否命中一个文档写事件（输入取自信封的
// DatabaseID/CollectionID 与 op）。
func (p EventPattern) Matches(databaseID, collectionID, op string) bool {
	if p.DatabaseID != databaseID {
		return false
	}
	if p.CollectionID != EventSegmentAny && p.CollectionID != collectionID {
		return false
	}
	if p.Op != EventSegmentAny && p.Op != op {
		return false
	}
	return true
}

// String 还原订阅串（诊断/测试用）。
func (p EventPattern) String() string {
	return fmt.Sprintf("databases.%s.collections.%s.documents.%s", p.DatabaseID, p.CollectionID, p.Op)
}

// ParseEventPattern 解析并校验一条订阅串（创建期校验与匹配器构建共用同一
// 解析——存储侧不再有第二套宽松口径，fail-closed）。
func ParseEventPattern(s string) (EventPattern, error) {
	if s == "" {
		return EventPattern{}, fmt.Errorf("empty event pattern")
	}
	if len(s) > MaxEventPatternBytes {
		return EventPattern{}, fmt.Errorf("event pattern exceeds maximum of %d bytes", MaxEventPatternBytes)
	}
	parts := strings.Split(s, ".")
	if len(parts) != 6 {
		return EventPattern{}, fmt.Errorf("must be databases.{database}.collections.{collection}.documents.{op}, got %d segments", len(parts))
	}
	if parts[0] != "databases" || parts[2] != "collections" || parts[4] != "documents" {
		return EventPattern{}, fmt.Errorf("fixed segments must be databases/collections/documents")
	}
	p := EventPattern{DatabaseID: parts[1], CollectionID: parts[3], Op: parts[5]}
	// database 段一期必须精确（设计 §4.1：database 级通配后置）。
	if p.DatabaseID == "" {
		return EventPattern{}, fmt.Errorf("database segment must not be empty")
	}
	if p.DatabaseID == EventSegmentAny {
		return EventPattern{}, fmt.Errorf("database wildcard is not supported yet (phase 1 requires exact database)")
	}
	if p.CollectionID == "" {
		return EventPattern{}, fmt.Errorf("collection segment must not be empty")
	}
	switch p.Op {
	case EventOpCreate, EventOpUpdate, EventOpDelete, EventSegmentAny:
	default:
		return EventPattern{}, fmt.Errorf("op must be create|update|delete|*, got %q", p.Op)
	}
	return p, nil
}

// ValidateEventPatterns 校验订阅串列表（app 层创建期调用）：非空、逐条可
// 解析。解析错误的文案携带具体条目（InvalidArgument 直出）。
func ValidateEventPatterns(events []string) error {
	if len(events) == 0 {
		return fmt.Errorf("events is required for event triggers")
	}
	if len(events) > MaxEventPatterns {
		return fmt.Errorf("events exceeds maximum of %d patterns", MaxEventPatterns)
	}
	for _, e := range events {
		if _, err := ParseEventPattern(e); err != nil {
			return fmt.Errorf("invalid event %q: %w", e, err)
		}
	}
	return nil
}

// EventSubscription 是匹配器内的一条已展开订阅（触发器 × 订阅串）。
type EventSubscription struct {
	ProjectID  string
	FunctionID string
	TriggerID  string
	Pattern    EventPattern
}

// Matches 报告该订阅是否命中某项目的一个文档写事件。
func (s EventSubscription) Matches(projectID, databaseID, collectionID, op string) bool {
	return s.ProjectID == projectID && s.Pattern.Matches(databaseID, collectionID, op)
}

// EventTriggerIndex 是进程内订阅匹配器（project → 订阅条目列表）：worker
// 周期快照扫描后整体换入（原子指针），消费路径零查库（v3 §4.2——匹配是
// 纯内存操作）。
type EventTriggerIndex struct {
	byProject map[string][]EventSubscription
	count     int
}

// NewEventTriggerIndex 由触发器列表构建匹配器（忽略非 event 类型与不可
// 解析的订阅串——后者理论不可达：创建期已校验；防御坏数据不阻塞索引构建，
// 与 cron 领取跳过坏表达式行同款取舍）。
func NewEventTriggerIndex(triggers []Trigger) *EventTriggerIndex {
	idx := &EventTriggerIndex{byProject: make(map[string][]EventSubscription)}
	for i := range triggers {
		t := &triggers[i]
		if t.Type != TriggerTypeEvent || !t.Enabled {
			continue
		}
		for _, raw := range t.Config.Events {
			p, err := ParseEventPattern(raw)
			if err != nil {
				continue
			}
			idx.byProject[t.ProjectID] = append(idx.byProject[t.ProjectID], EventSubscription{
				ProjectID:  t.ProjectID,
				FunctionID: t.FunctionID,
				TriggerID:  t.ID,
				Pattern:    p,
			})
			idx.count++
		}
	}
	return idx
}

// Match 返回某项目内命中该文档写事件的全部订阅（逐 trigger 逐条投递；
// 同一触发器多条订阅串命中同一事件 → 多次投递，与「订阅串数 = 触发面」
// 的 Appwrite 语义一致，函数侧按 event_id 幂等吸收）。
func (x *EventTriggerIndex) Match(projectID, databaseID, collectionID, op string) []EventSubscription {
	if x == nil {
		return nil
	}
	subs := x.byProject[projectID]
	if len(subs) == 0 {
		return nil
	}
	out := make([]EventSubscription, 0, len(subs))
	for _, s := range subs {
		if s.Matches(projectID, databaseID, collectionID, op) {
			out = append(out, s)
		}
	}
	return out
}

// Len 返回索引内的订阅总数（观测/日志用）。
func (x *EventTriggerIndex) Len() int {
	if x == nil {
		return 0
	}
	return x.count
}

// Empty 报告索引是否没有任何订阅。
func (x *EventTriggerIndex) Empty() bool { return x.Len() == 0 }

// EventOpFromEnvelope 从信封的事件名（databases.documents.create 等）提取
// op 段；非文档写事件（未知形态）返回空串——消费路径据此跳过。
func EventOpFromEnvelope(event string) string {
	idx := strings.LastIndexByte(event, '.')
	if idx < 0 || idx == len(event)-1 {
		return ""
	}
	op := event[idx+1:]
	switch op {
	case EventOpCreate, EventOpUpdate, EventOpDelete:
		return op
	default:
		return ""
	}
}
