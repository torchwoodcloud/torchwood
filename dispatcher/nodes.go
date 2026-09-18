package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 节点注册表（四期 4a-1，多机细胞模型，设计
// docs/design/functions-runtimes-and-sources.md §4 M2 + M8）。
//
// 拓扑：N 节点 ×（dispatcher 进程 + 本机 docker daemon），控制面共享
// （Redis：实例注册表/spawn 锁/节点注册表）。每个 dispatcher 进程启动即
// 自注册 + 周期心跳（SET torchwood:fnnodes:<node_id> + TTL，心跳刷新）；
// TTL 到期 = 节点失联。reaper 据此做两件事（M8，与节点注册同片交付——
// 没有它，多节点共享 fninst 后 reaper 互相残杀）：
//  1. 对账范围收窄：只处理 rec.Node == 本节点 的记录（本机 daemon
//     Inspect 对他节点容器必然 NotFound，误入幽灵清理会把健康实例从池中
//     蒸发——容器泄漏、容量骤降）；
//  2. 死节点收敛：心跳键已消失（≥TTL 无心跳 = 节点已死，容器随机器消失）
//     的实例记录由任意存活节点批量清除（只删记录、不碰 daemon；与幽灵
//     清理严格区分）。idle 记录无 lease 过期回收的现状（只有 busy stuck
//     有 grace）由此路径补齐。
const (
	// nodeKeyPrefix 是节点注册表键前缀：torchwood:fnnodes:<node_id> 为
	// SET（value = NodeRecord JSON，TTL 心跳刷新）。
	nodeKeyPrefix = "torchwood:fnnodes:"

	// defaultNodeTTL 是节点心跳 TTL（心跳间隔的 3 倍容忍度）：最后一次
	// 心跳后 30s 键自动消失 = 节点失联判定基准（M8 死节点收敛的宽限）。
	defaultNodeTTL = 30 * time.Second

	// nodeHeartbeatInterval 是心跳周期（TTL 的 1/3：连续两次失败仍能在
	// TTL 内刷新成功）。
	nodeHeartbeatInterval = 10 * time.Second

	// nodeHBTimeout 是单次心跳写操作的独立超时（心跳不得占用 reaper 的
	// dockerCleanupTimeout 预算）。
	nodeHBTimeout = 5 * time.Second

	// defaultDispatcherPort 是 addr 缺省时的监听端口（service.go 同值）。
	defaultDispatcherPort = "9070"
)

// NodeRecord 是节点注册表成员：一个 dispatcher 节点的投影。时间字段沿用
// 实例注册表约定：数值 Unix 毫秒（无 Lua 改写路径，纯一致性口径）。
type NodeRecord struct {
	// NodeID 是节点标识（config functions.dispatcher.node_id 或 hostname）。
	NodeID string `json:"node_id"`
	// URL 是本节点对等互达的 dispatcher URL（config node_url，或按 addr
	// 端口推导 http://127.0.0.1:<port>——仅单机成立；M3 路由层 4a-2 消费）。
	URL string `json:"url"`
	// HbUnixMS 是最近一次心跳时刻（Unix 毫秒；观测用，失联判定以键 TTL
	// 为准——键消失即失联，不依赖时间戳比对）。
	HbUnixMS int64 `json:"hb_unix_ms"`
}

// NodeRegistry 是节点注册表端口（M2）；Registry 组合本接口（PoolManager/
// service 经同一 Registry 使用，fake 测试用内存实现）。
type NodeRegistry interface {
	// SaveNode 注册/续期节点心跳（SET + TTL；幂等，心跳路径周期调用）。
	SaveNode(ctx context.Context, rec NodeRecord, ttl time.Duration) error
	// GetNode 读取节点记录；不存在返回 (nil, nil)（与 ClaimIdle 同款惯例）。
	GetNode(ctx context.Context, nodeID string) (*NodeRecord, error)
	// DeleteNode 注销节点（幂等；正常关停不需调用——TTL 自然过期）。
	DeleteNode(ctx context.Context, nodeID string) error
	// ListNodes 枚举当前存活（心跳未过期）的全部节点（SCAN）。
	ListNodes(ctx context.Context) ([]NodeRecord, error)
}

// nodeKey 拼节点注册表键。
func nodeKey(nodeID string) string { return nodeKeyPrefix + nodeID }

// ——Redis 实现（挂在 redisRegistry 上，见 registry.go）——

func (r *redisRegistry) SaveNode(ctx context.Context, rec NodeRecord, ttl time.Duration) error {
	raw, err := jsonMarshal(rec)
	if err != nil {
		return fmt.Errorf("encode node record: %w", err)
	}
	return r.rdb.Set(ctx, nodeKey(rec.NodeID), raw, ttl).Err()
}

func (r *redisRegistry) GetNode(ctx context.Context, nodeID string) (*NodeRecord, error) {
	raw, err := r.rdb.Get(ctx, nodeKey(nodeID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node: %w", err)
	}
	return decodeNodeRecord(raw)
}

func (r *redisRegistry) DeleteNode(ctx context.Context, nodeID string) error {
	return r.rdb.Del(ctx, nodeKey(nodeID)).Err()
}

func (r *redisRegistry) ListNodes(ctx context.Context) ([]NodeRecord, error) {
	var out []NodeRecord
	var cursor uint64
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, nodeKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan node registry: %w", err)
		}
		for _, k := range keys {
			raw, err := r.rdb.Get(ctx, k).Result()
			if err != nil {
				continue // SCAN 与 GET 之间过期的键：跳过（下一轮心跳自愈）
			}
			rec, err := decodeNodeRecord(raw)
			if err != nil {
				continue // 半写/损坏记录跳过，不得毒化整个列表
			}
			out = append(out, *rec)
		}
		if next == 0 {
			return out, nil
		}
		cursor = next
	}
}

func decodeNodeRecord(raw string) (*NodeRecord, error) {
	var rec NodeRecord
	if err := jsonUnmarshal([]byte(raw), &rec); err != nil {
		return nil, err
	}
	if rec.NodeID == "" {
		return nil, errors.New("node record missing id")
	}
	return &rec, nil
}

// ResolveNodeIdentity 解析本 dispatcher 进程的节点身份（M2；service 装配处
// 调用）：
//   - node_id：config functions.dispatcher.node_id，空 = os.Hostname 兜底
//     （hostname 仍为空等极端环境回落静态占位，保证心跳键非空）；
//   - node_url：config functions.dispatcher.node_url，空 = 按 addr 端口推导
//     "http://127.0.0.1:<port>"。推导值只对同机调用方成立——多机部署必须
//     显式配置 node_url（对等互达地址无法从监听地址推导，proto 注释同声明）。
func ResolveNodeIdentity(cfg *config.AppConfig) (nodeID, nodeURL string) {
	d := cfg.GetFunctions().GetDispatcher()
	nodeID = strings.TrimSpace(d.GetNodeId())
	if nodeID == "" {
		if h, err := os.Hostname(); err == nil {
			nodeID = strings.TrimSpace(h)
		}
		if nodeID == "" {
			nodeID = "dispatcher-node"
		}
	}
	nodeURL = strings.TrimSpace(d.GetNodeUrl())
	if nodeURL == "" {
		addr := d.GetAddr()
		if addr == "" {
			addr = ":" + defaultDispatcherPort
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil || port == "" {
			port = defaultDispatcherPort
		}
		nodeURL = "http://127.0.0.1:" + port
	}
	return nodeID, nodeURL
}
