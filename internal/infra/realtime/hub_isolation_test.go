package realtime

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
)

// TestHub_DispatchCrossProjectIsolated（B-1 回归）：频道名（topic）不含
// project 维度，跨项目同名集合订阅同一全局频道。扇出必须按连接归属项目
// 过滤：文档事件 ev.ProjectID != conn.ProjectID 时不得投递（在他项目事件
// 上不做 ACL 评估，直接跳）。
func TestHub_DispatchCrossProjectIsolated(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)

	evA := testEnvelope() // ProjectID: "default"

	// 关键场景：同一文档主体（user:u1，ACL 均可见），仅连接归属项目不同。
	connA := newTestConn("conn-a", databases.Principal{Roles: []string{"users", "user:u1"}})
	connA.ProjectID = "default"
	connB := newTestConn("conn-b", databases.Principal{Roles: []string{"users", "user:u1"}})
	connB.ProjectID = "proj-b"

	ch := evA.CollectionChannel()
	h.Subscribe(ch, connA)
	h.Subscribe(ch, connB)

	// A 项目事件：A 收到；B 不得收到（即便集合权限 read:any 对其 ACL 通过）。
	h.Dispatch(evA)
	require.Len(t, drain(t, connA.Send), 1, "同项目事件正常投递")
	require.Len(t, drain(t, connB.Send), 0, "B-1：他项目事件不得投递到同名频道订阅者")

	// 反向：B 项目事件只投 B。
	evB := testEnvelope()
	evB.EventID = "01J-test-event-b"
	evB.ProjectID = "proj-b"
	h.Dispatch(evB)
	require.Len(t, drain(t, connA.Send), 0, "B-1：他项目事件不得投递到同名频道订阅者")
	require.Len(t, drain(t, connB.Send), 1, "同项目事件正常投递")
}

// TestHub_DispatchCrossProjectPlatformAdminIsolated（B-1）：platform admin
// 旁路 ACL，但不旁路项目隔离——admin 连接绑定 hello.project_id（握手信任锚），
// 只收该项目的文档事件；要看他项目事件须以该项目开连接。
func TestHub_DispatchCrossProjectPlatformAdminIsolated(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)

	admin := newTestConn("adm", databases.Principal{Roles: []string{"admin"}})
	admin.PlatformAdmin = true
	admin.ProjectID = "default"
	h.Subscribe(testEnvelope().CollectionChannel(), admin)

	ev := testEnvelope()
	h.Dispatch(ev)
	require.Len(t, drain(t, admin.Send), 1, "同项目事件 admin 正常收")

	other := testEnvelope()
	other.EventID = "01J-test-event-other-project"
	other.ProjectID = "proj-b"
	h.Dispatch(other)
	require.Len(t, drain(t, admin.Send), 0, "admin 亦受项目隔离约束")
}

// TestHub_DispatchEmptyConnProjectFailsClosed（B-1 防御）：连接无项目归属
// （生产握手路径不可达；防御零值误用）时 fail-closed——不投递任何文档事件。
func TestHub_DispatchEmptyConnProjectFailsClosed(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)

	conn := newTestConn("no-proj", databases.Principal{Roles: []string{"users", "user:u1"}})
	conn.ProjectID = "" // 显式清空（newTestConn 默认 "default"）
	h.Subscribe(testEnvelope().CollectionChannel(), conn)

	h.Dispatch(testEnvelope())
	require.Len(t, drain(t, conn.Send), 0, "无项目归属的连接 fail-closed 不投递")
}
