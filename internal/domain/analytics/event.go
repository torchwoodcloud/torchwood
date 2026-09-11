package analytics

import (
	"encoding/json"
	"time"
)

// Source 是事件摄入通道（存储列 source 的值域）。
const (
	// SourceClient client 面（principal 归因，不可伪造，D3）。
	SourceClient = "client"
	// SourceServer server 面（API Key，可信代报 user_id）。
	SourceServer = "server"
)

// Event 是待落库的领域事件（app 用例层完成校验/钳制/归因/截断后的形态；
// Props 为已标量化+截断后的规范 JSON 对象，空集以 '{}' 表达）。
type Event struct {
	Name       string
	UserID     string // client 面来自 principal；server 面可信代报；空 = 无归属
	SessionID  string
	Source     string // SourceClient | SourceServer
	Platform   string
	AppVersion string
	OccurredAt time.Time // 缺省已在用例层落定为服务端 now 并过钳制窗
	IngestedAt time.Time // 服务端接收时间（双时间列留档，D4）
	Props      json.RawMessage
}

// EventDefinition 是事件字典行（D12：摄取时 upsert，发现即时）。
type EventDefinition struct {
	Name      string
	FirstSeen time.Time
	LastSeen  time.Time
}
