package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件覆盖四期 4a-1 多机细胞模型数据面（设计
// docs/design/functions-runtimes-and-sources.md §4 M2/M5/M8）：
//   - 节点注册表（M2）：fake/Redis 双实现的 Save/Get/Delete/List 往返 + TTL；
//   - 节点身份解析（ResolveNodeIdentity）：config 缺省 → hostname / 端口推导；
//   - reaper 对账收窄 + 死节点收敛（M8）：他节点记录跳过 Inspect 与一切清理、
//     心跳消失的他节点记录被本节点收敛（只删记录不碰 daemon）、旧记录（node
//     为空）按本节点自然老化、心跳快照失败 fail-safe；
//   - BuildResponse.node_id 与 spawnInstance 节点固化、service 心跳。

// ——fake 节点注册表（接口实现；生产 Redis 实现在 nodes.go）——

func (r *fakeRegistry) SaveNode(_ context.Context, rec NodeRecord, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[rec.NodeID] = rec
	return nil
}

func (r *fakeRegistry) GetNode(_ context.Context, nodeID string) (*NodeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[nodeID]; ok {
		return &rec, nil
	}
	return nil, nil
}

func (r *fakeRegistry) DeleteNode(_ context.Context, nodeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, nodeID)
	return nil
}

func (r *fakeRegistry) ListNodes(_ context.Context) ([]NodeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodeErr != nil {
		return nil, r.nodeErr
	}
	out := make([]NodeRecord, 0, len(r.nodes))
	for _, rec := range r.nodes {
		out = append(out, rec)
	}
	return out, nil
}

// TestRegistry_NodesRoundTrip fake 往返：Save → Get → List → Delete 幂等，
// 心跳续期覆盖旧记录。
func TestRegistry_NodesRoundTrip(t *testing.T) {
	ctx := context.Background()
	reg := newFakeRegistry()

	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "n1", URL: "http://n1:9070", HbUnixMS: 111}, defaultNodeTTL))
	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "n2", URL: "http://n2:9070", HbUnixMS: 222}, defaultNodeTTL))

	got, err := reg.GetNode(ctx, "n1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "http://n1:9070", got.URL)

	// 心跳续期：同 id 覆盖。
	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "n1", URL: "http://n1:9070", HbUnixMS: 333}, defaultNodeTTL))
	got, err = reg.GetNode(ctx, "n1")
	require.NoError(t, err)
	require.Equal(t, int64(333), got.HbUnixMS)

	missing, err := reg.GetNode(ctx, "nope")
	require.NoError(t, err)
	require.Nil(t, missing, "不存在的节点返回 (nil, nil)")

	nodes, err := reg.ListNodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 2)

	require.NoError(t, reg.DeleteNode(ctx, "n1"))
	nodes, err = reg.ListNodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, "n2", nodes[0].NodeID)
	require.NoError(t, reg.DeleteNode(ctx, "n1"), "DeleteNode 幂等")
}

// TestRedisRegistry_NodesRoundTrip Redis 实现往返（miniredis 兜底，见
// newRegistryTestRedis）：SET + TTL 键形态、SCAN 列表、过期即失联。
func TestRedisRegistry_NodesRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()

	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "rn1", URL: "http://rn1:9070", HbUnixMS: 42}, defaultNodeTTL))
	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "rn2", URL: "http://rn2:9070", HbUnixMS: 43}, defaultNodeTTL))

	// 键形态与 TTL：torchwood:fnnodes:<node_id> SET，TTL ≤ defaultNodeTTL。
	raw, err := rdb.Get(ctx, nodeKey("rn1")).Result()
	require.NoError(t, err)
	var decoded NodeRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
	require.Equal(t, "rn1", decoded.NodeID)
	require.Equal(t, "http://rn1:9070", decoded.URL)
	ttl, err := rdb.TTL(ctx, nodeKey("rn1")).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0), "心跳键必须带 TTL")
	require.LessOrEqual(t, ttl, defaultNodeTTL)

	got, err := reg.GetNode(ctx, "rn1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, int64(42), got.HbUnixMS)

	missing, err := reg.GetNode(ctx, "rn-gone")
	require.NoError(t, err)
	require.Nil(t, missing)

	nodes, err := reg.ListNodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 2, "SCAN 列表不得丢节点")

	// 键过期 = 心跳消失（M8 失联判定的真实语义）。
	require.NoError(t, reg.DeleteNode(ctx, "rn2"))
	nodes, err = reg.ListNodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, "rn1", nodes[0].NodeID)
}

// TestResolveNodeIdentity 节点身份解析表驱动：config 显式值优先；node_id
// 缺省回落 hostname；node_url 缺省按 addr 端口推导（多机必须显式配置）。
func TestResolveNodeIdentity(t *testing.T) {
	hostname := ""
	if h, err := os.Hostname(); err == nil {
		hostname = h
	}
	cases := []struct {
		name           string
		dispatcher     *config.Functions_Dispatcher
		wantID         string
		wantURL        string
		wantIDIsHost   bool
		wantURLDerived bool
	}{
		{
			name:           "全缺省：hostname + addr 端口推导",
			dispatcher:     nil,
			wantIDIsHost:   hostname != "",
			wantURL:        "http://127.0.0.1:9070",
			wantURLDerived: true,
		},
		{
			name:           "仅 addr 缺省：端口推导用 9070 缺省",
			dispatcher:     &config.Functions_Dispatcher{NodeId: "n1"},
			wantID:         "n1",
			wantURL:        "http://127.0.0.1:9070",
			wantURLDerived: true,
		},
		{
			name: "addr 带主机名：端口推导取端口",
			dispatcher: &config.Functions_Dispatcher{
				NodeId: "n1", NodeUrl: "", Addr: "0.0.0.0:9123",
			},
			wantID:         "n1",
			wantURL:        "http://127.0.0.1:9123",
			wantURLDerived: true,
		},
		{
			name: "显式 node_url 优先（多机形态）",
			dispatcher: &config.Functions_Dispatcher{
				NodeId: "dispatcher-1", NodeUrl: "http://dispatcher-1:9070", Addr: ":9070",
			},
			wantID:  "dispatcher-1",
			wantURL: "http://dispatcher-1:9070",
		},
		{
			name: "node_id 空白串视同缺省（hostname 兜底）",
			dispatcher: &config.Functions_Dispatcher{
				NodeId: "   ",
			},
			wantIDIsHost:   hostname != "",
			wantURL:        "http://127.0.0.1:9070",
			wantURLDerived: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.AppConfig{}
			if tc.dispatcher != nil {
				cfg.Functions = &config.Functions{Dispatcher: tc.dispatcher}
			}
			nodeID, nodeURL := ResolveNodeIdentity(cfg)
			if tc.wantIDIsHost {
				require.Equal(t, hostname, nodeID, "node_id 缺省必须回落 os.Hostname")
			} else {
				require.Equal(t, tc.wantID, nodeID)
			}
			if tc.wantURLDerived {
				require.True(t, strings.HasPrefix(nodeURL, "http://127.0.0.1:"), "推导形态 = http://127.0.0.1:<port>")
				require.True(t, strings.HasSuffix(nodeURL, ":"+strings.TrimPrefix(tc.wantURL, "http://127.0.0.1:")))
			} else {
				require.Equal(t, tc.wantURL, nodeURL)
			}
		})
	}
}

// seedForeignRecord 在注册表放一条他节点的实例记录。
func seedForeignRecord(t *testing.T, reg *fakeRegistry, ref FunctionRef, instanceID, node string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, reg.Save(context.Background(), ref, InstanceRecord{
		InstanceID:   instanceID,
		ContainerID:  instanceID,
		IP:           "10.7.0.9",
		DeploymentID: "dep1",
		Node:         node,
		SpawnedAtMS:  now.UnixMilli(),
		IdleSinceMS:  now.UnixMilli(),
		LeaseUntilMS: now.Add(time.Minute).UnixMilli(),
	}))
}

// TestPoolReaper_ForeignLiveNodeSkipped M8 ①：他节点（心跳存活）的记录被
// 本节点 reaper 完全跳过——不 Inspect、不清理、不进水位计量。
func TestPoolReaper_ForeignLiveNodeSkipped(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	pool.SetNodeID("node-a")
	ctx := context.Background()
	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "node-b", URL: "http://b:9070"}, defaultNodeTTL))

	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	seedForeignRecord(t, reg, ref, "foreign-1", "node-b")
	// 容器在本机 daemon 上不存在：若收窄失效（误 Inspect），会走幽灵清理
	// 把他节点健康实例从池中蒸发——这正是 M8 要堵的回归。
	pool.Reaper(ctx)

	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Len(t, records, 1, "他节点存活心跳的记录不得被本节点触碰")
	require.NotContains(t, d.inspects, "foreign-1", "他节点记录不得 Inspect（本机 daemon 必 NotFound）")
	require.Empty(t, d.stopped)
	require.Empty(t, d.removed)
}

// TestPoolReaper_DeadNodeConverged M8 ②：他节点心跳消失（键不存在）→ 其
// 实例记录被本节点批量收敛删除——只删记录，不碰 daemon（容器随死节点已
// 消失，记录清理无需本机 daemon）。P2 S13 二次确认：单轮快照缺失只登记
// （deadNodePending），连续两轮快照缺失才动手。
func TestPoolReaper_DeadNodeConverged(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	pool.SetNodeID("node-a")
	ctx := context.Background()
	// node-b 不注册心跳（TTL 已过期的等价形态）；node-c 心跳存活作对照。

	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	seedForeignRecord(t, reg, ref, "dead-node-rec", "node-b")
	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "node-c", URL: "http://c:9070"}, defaultNodeTTL))
	seedForeignRecord(t, reg, ref, "live-node-rec", "node-c")

	// 第一轮：node-b 首次缺失 → 只登记，不删（瞬时抖动误判窗口的降噪）。
	pool.Reaper(ctx)
	ids := recordIDs(t, reg, ref)
	require.True(t, ids["dead-node-rec"], "单轮快照缺失不得删记录（二次确认）")
	require.True(t, ids["live-node-rec"], "存活节点记录原样保留")

	// 第二轮：连续缺失 → 确认收敛。
	pool.Reaper(ctx)
	ids = recordIDs(t, reg, ref)
	require.False(t, ids["dead-node-rec"], "连续两轮缺失后死节点记录被收敛")
	require.True(t, ids["live-node-rec"], "仅死节点的记录被收敛")
	require.Empty(t, d.stopped, "死节点收敛只删记录、绝不碰本机 daemon")
	require.Empty(t, d.removed)
	require.NotContains(t, d.inspects, "dead-node-rec")
}

// TestPoolReaper_DeadNodeConfirmResetsOnRevival 二次确认状态机（P2 S13）：
// 节点复活（心跳回归）清空缺失登记——再次失联须重新走两轮确认；快照失败
// 不登记不确认（fail-safe，与收敛跳过同口径）。
func TestPoolReaper_DeadNodeConfirmResetsOnRevival(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	pool.SetNodeID("node-a")
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	seedForeignRecord(t, reg, ref, "flap-rec", "node-b")

	// 第一轮缺失：登记。
	pool.Reaper(ctx)
	require.True(t, recordIDs(t, reg, ref)["flap-rec"])

	// 节点复活：登记清账；记录照常保留。
	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "node-b", URL: "http://b:9070"}, defaultNodeTTL))
	pool.Reaper(ctx)
	require.True(t, recordIDs(t, reg, ref)["flap-rec"])

	// 再次失联：又要两轮才收敛（复活清账生效）。
	require.NoError(t, reg.DeleteNode(ctx, "node-b"))
	pool.Reaper(ctx)
	require.True(t, recordIDs(t, reg, ref)["flap-rec"], "复活后的首次缺失重新只登记")
	pool.Reaper(ctx)
	require.False(t, recordIDs(t, reg, ref)["flap-rec"], "复活后再连续两轮缺失才收敛")

	// 快照失败：不登记、不确认。
	seedForeignRecord(t, reg, ref, "flap2-rec", "node-c")
	reg.nodeErr = errors.New("redis boom")
	pool.Reaper(ctx)
	pool.Reaper(ctx)
	require.True(t, recordIDs(t, reg, ref)["flap2-rec"], "快照失败期间死节点收敛整体跳过")
	reg.nodeErr = nil
	pool.Reaper(ctx)
	require.True(t, recordIDs(t, reg, ref)["flap2-rec"], "快照恢复后的首轮只登记")
	pool.Reaper(ctx)
	require.False(t, recordIDs(t, reg, ref)["flap2-rec"], "快照恢复后再一轮缺失即收敛")
}

// TestNodeHeartbeatTimingContract 心跳节拍契约（P2 S13 放宽）：TTL 90s /
// 间隔 15s——连续两次心跳失败（2×间隔 = 30s 空白）仍远在 TTL 内，容量键
// TTL 与心跳同宽。
func TestNodeHeartbeatTimingContract(t *testing.T) {
	require.Equal(t, 90*time.Second, defaultNodeTTL, "TTL 必须为 90s（30s 只容忍 2 次连续失败，抖动即误判）")
	require.Equal(t, 15*time.Second, nodeHeartbeatInterval, "间隔必须为 15s")
	require.Equal(t, defaultNodeTTL, capacityKeyTTL, "容量键 TTL 与心跳同宽")
	require.Less(t, 2*nodeHeartbeatInterval, defaultNodeTTL, "连续两次失败必须仍在 TTL 内")
	require.Equal(t, 6*nodeHeartbeatInterval, defaultNodeTTL, "TTL = 6×间隔（任务口径 90/15）")
}

// TestPoolReaper_LegacyRecordTreatedAsSelf 旧记录兼容：node 为空（本特性
// 之前的单机存量）按本节点对账——幽灵清理照常收敛（自然老化，无升级
// runbook；若按「空 ≠ self」处理会被死节点收敛批量蒸发）。
func TestPoolReaper_LegacyRecordTreatedAsSelf(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	pool.SetNodeID("node-a")
	ctx := context.Background()

	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	seedForeignRecord(t, reg, ref, "legacy-1", "")
	// 容器已消失：本节点幽灵清理路径应照常收敛。
	pool.Reaper(ctx)

	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Empty(t, records, "node 为空的旧记录按本节点幽灵对账清理")
}

// TestPoolReaper_SelfNodeGhostCleanedStillWorks 本节点（显式 node id）记录
// 的既有对账语义不回归：幽灵清理照旧。
func TestPoolReaper_SelfNodeGhostCleanedStillWorks(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	pool.SetNodeID("node-a")
	ctx := context.Background()

	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	seedForeignRecord(t, reg, ref, "self-1", "node-a")
	// 容器不在本机 daemon 上 = 幽灵：Inspect NotFound → 幽灵清理。
	require.NoError(t, reg.SaveNode(ctx, NodeRecord{NodeID: "node-a", URL: "http://a:9070"}, defaultNodeTTL))
	pool.Reaper(ctx)

	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Empty(t, records, "自节点幽灵照旧清理")
	require.Contains(t, d.inspects, "self-1")
}

// TestPoolReaper_ListNodesFailSafeFailClosed 心跳快照读取失败 → 跳过死节点
// 收敛（fail-safe：不得因 Redis 抖动批量误删活节点记录），他节点记录原样
// 保留。
func TestPoolReaper_ListNodesFailSafeFailClosed(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	pool.SetNodeID("node-a")
	reg.nodeErr = errors.New("redis scan boom")
	ctx := context.Background()

	ref := FunctionRef{ProjectID: "p1", FunctionID: "fn1"}
	seedForeignRecord(t, reg, ref, "foreign-1", "node-b")
	pool.Reaper(ctx)

	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Len(t, records, 1, "快照失败时死节点收敛必须跳过")
	require.Empty(t, d.stopped)
	require.Empty(t, d.removed)
}

// TestSpawnInstanceRecordsNode spawn 固化节点归属：InstanceRecord.Node =
// 本进程节点 ID（M3/M8 的对账依据）。
func TestSpawnInstanceRecordsNode(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, nil)
	pool.SetNodeID("node-a")
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	records, _ := reg.List(ctx, FunctionRef{ProjectID: "p1", FunctionID: "fn1"})
	require.Len(t, records, 1)
	require.Equal(t, "node-a", records[0].Node, "spawn 必须把本节点 ID 固化进记录")
}

// TestServer_BuildCarriesNodeID BuildResponse.node_id（M5 构建亲和通道）：
// handleBuild 成功响应携带 dispatcher 自身节点 ID，server 侧落
// deployment.build_node。
func TestServer_BuildCarriesNodeID(t *testing.T) {
	srv, _, pool := newTestServer(t, "")
	pool.SetNodeID("node-a")
	rec := postJSON(t, srv, "/v1/dispatch/builds", "", BuildRequest{
		ProjectID:    "p1",
		FunctionID:   "fn1",
		DeploymentID: "dep1",
		ZipBase64:    "UEsDBAoAAAAAAA==",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp BuildResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "node-a", resp.NodeID, "BuildResponse 必须携带本节点 ID")
}

// TestService_BeatNode 节点自注册心跳（M2）：beatNode 写入节点记录
// （id/url/时间戳齐备）；未装配注册表时不 panic（防御）。
func TestService_BeatNode(t *testing.T) {
	reg := newFakeRegistry()
	s := &Service{registry: reg, nodeID: "node-a", nodeURL: "http://node-a:9070", logger: slog.Default()}
	s.beatNode()

	got, err := reg.GetNode(context.Background(), "node-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "http://node-a:9070", got.URL)
	require.NotZero(t, got.HbUnixMS)
}
