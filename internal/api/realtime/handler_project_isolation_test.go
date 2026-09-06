package realtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainevents "github.com/torchwooddev/torchwood/internal/domain/events"
)

// TestSubscribe_CrossProjectEventNotDelivered（B-1 端到端回归）：频道是
// 全局命名空间（databases.<db>.collections.<coll> 不含 project 维度），
// WS 订阅门控校验的是自己项目的集合、实际订阅的是全局频道——隔离必须
// 在扇出侧收口：连接归属项目（hello.project_id，握手已与凭证一致校验）
// 与事件 ProjectID 不等时不得投递（含文档全文），同项目事件正常投递。
func TestSubscribe_CrossProjectEventNotDelivered(t *testing.T) {
	docDB := &fakeDocDB{collections: map[string]*databases.Collection{}}
	setupCollection(docDB, "app", "posts", false, false)
	hub, srv := replayHarness(t, &replayDocDB{fakeDocDB: docDB})

	// 项目 default 的连接订阅同名集合频道。
	c := replayDial(t, srv)
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s1", "channel": "databases.app.collections.posts"})
	var ack struct {
		Type string `json:"type"`
	}
	readTestFrame(t, c, &ack)
	require.Equal(t, "subscribed", ack.Type)

	// 他项目（proj-b）事件：同频道、ACL 对本连接主体可见——不得投递。
	// 先于 own dispatch：连接 Send 通道与写循环均 FIFO，若被投递必先于
	// own-1 上线（coder/websocket 的 Read ctx 超时会关闭连接，无法先做
	// 负读窗口再正常收帧——首帧断言即等价负向证明）。
	other := domainevents.Envelope{
		EventID: "other-1", Event: domainevents.EventDocumentsUpdate,
		ProjectID: "proj-b", DatabaseID: "app", CollectionID: "posts",
		DocumentID: "p1", Version: 1, CreatedAt: time.Now(), Seq: 2,
		ACL: domainevents.ACLSnapshot{
			DocumentSecurity:    true,
			DocumentPermissions: []databases.Permission{{Type: "read", Role: "user:u1"}},
			DocHasPerms:         true,
		},
	}
	hub.Dispatch(other)

	// 同项目事件正常投递。
	own := other
	own.EventID = "own-1"
	own.ProjectID = "default"
	own.Seq = 3
	hub.Dispatch(own)

	var ev struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, c, &ev)
	require.Equal(t, "event", ev.Type)
	require.Equal(t, "own-1", ev.Payload["event_id"],
		"首帧必须是同项目事件——他项目事件若被投递必先上线（FIFO）")
	require.Equal(t, "default", ev.Payload["project_id"])
}
