package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
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
//
// 节点容量键（四期 4c M4 容量共享，同文件交付）：torchwood:fncap:resident:
// <node_id> SET+TTL——值 = 该节点进程内常驻计数（spawn 成功 / terminate /
// reaper 刷新），全局上限 = 全部节点键之和（SCAN 求和）；Redis 不可用时
// 全局检查 fail-open 退化为仅本节点上限（容量门不是安全门）。
const (
	// nodeKeyPrefix 是节点注册表键前缀：torchwood:fnnodes:<node_id> 为
	// SET（value = NodeRecord JSON，TTL 心跳刷新）。
	nodeKeyPrefix = "torchwood:fnnodes:"

	// nodeCapacityKeyPrefix 是节点容量键前缀（四期 4c M4 容量共享，设计 §4）：
	// torchwood:fncap:resident:<node_id> 为 SET（value = 该节点当前常驻实例
	// 数的十进制文本，TTL 与心跳同宽）。全局常驻总量 = 全部节点键之和
	//（SCAN 求和；节点数小，可接受）——max_resident_instances 只是每节点
	// 进程内计数互不知，全局配额必须跨节点可见。
	nodeCapacityKeyPrefix = "torchwood:fncap:resident:"

	// capacityKeyTTL 是节点容量键的 TTL（与节点心跳 defaultNodeTTL 同宽）：
	// 刷新点 = spawn 成功 / terminate / reaper（reaper 每轮无条件刷一次，
	// ReaperInterval 15s = TTL 的 1/6，连续多轮失败仍在 TTL 内）。节点死后
	// 键在 TTL 内消失，全局求和自动不再计入死节点（心跳死亡判定同款语义）。
	capacityKeyTTL = defaultNodeTTL

	// capacityWriteTimeout 是容量键单次写操作的独立超时（与 nodeHBTimeout
	// 同款纪律：容量刷新不得占用调用路径的超时预算）。
	capacityWriteTimeout = 5 * time.Second

	// defaultNodeTTL 是节点心跳 TTL（心跳间隔的 6 倍容忍度）：最后一次
	// 心跳后 90s 键自动消失 = 节点失联判定基准（M8 死节点收敛的宽限）。
	// P2 S13 由 30s 放宽：30s/10s 只容忍 2 次连续心跳失败，瞬时 Redis
	// 抖动/网络闪断即误判节点死亡、触发存活节点的记录误删（容器还活着
	// → 泄漏）；90s/15s 容忍连续 2 次失败仍余 4 个心跳周期，配合 reaper
	// 死节点二次确认（连续两轮快照缺失，pool.go deadNodePending）双重降噪。
	defaultNodeTTL = 90 * time.Second

	// nodeHeartbeatInterval 是心跳周期（TTL 的 1/6：连续两次失败仍有充足
	// 余量在 TTL 内刷新成功）。
	nodeHeartbeatInterval = 15 * time.Second

	// nodeHBTimeout 是单次心跳写操作的独立超时（心跳不得占用 reaper 的
	// dockerCleanupTimeout 预算）。
	nodeHBTimeout = 5 * time.Second

	// defaultDispatcherPort 是 addr 缺省时的监听端口（service.go 同值）。
	defaultDispatcherPort = "9070"
)

// ——多副本/容器化部署前置条件（P2 S13 登记；现状单机 Dokploy 部署均不
// 触发，扩副本或容器化重建 dispatcher 前逐项核对）——
//
//  1. node_id / node_url 必须显式配置。ResolveNodeIdentity 缺省回落
//     os.Hostname：容器化部署 hostname 随容器重建变化，node_id 漂移后旧
//     实例记录（旧 node）要等旧「节点」失联才被收敛，期间对账/认领按孤儿
//     处理；node_url 缺省推导 http://127.0.0.1:<port> 只对同机调用方成立，
//     多机转发必败。二者均无静态默认可依——部署清单强制落配置。
//  2. Redis 是单点：fninst（实例注册表）/ fnnodes（节点心跳）/
//     fncap（容量键）/ fnspawn（spawn 锁）全部依赖单一 Redis 实例，无
//     HA/仲裁。Redis 不可达时的降级语义分散在各文件头（容量门 fail-open、
//     死节点收敛 fail-safe 跳过、spawn 锁获取失败即 spawn 失败），扩副本前
//     需按部署形态评估（哨兵/集群或接受单点）。
//  3. S13 修复的语义边界：
//     - ClaimIdle 按 node 收窄（registry.go claimIdleLua）：显式他节点记录
//       不认领；node 为空的旧记录放行（升级窗口共存语义，自然老化）；
//     - killInstance 归属校验（pool.go）：执行错误/drain/idle 回收路径不删
//       他节点记录——留给对端 reaper 收敛；死节点收敛（Reaper M8 ②）不经
//       killInstance，仍可删他节点记录（连续两轮快照缺失二次确认后）；
//     - reaper draining 兜底判杀分 busy/idle 两支（pool.go）：busy 走判活
//       规则（租约过期超 stuckBusyGrace），idle 按 release 刷新过的
//       idle_since_ms 判 30s 未自退出——DrainForDeployment 的在途宽限不再
//       被压到 30s；
//     - 心跳 defaultNodeTTL=90s / 间隔 15s：容忍连续 2 次心跳失败。
//  4. 已知残余（有意不做）：route 的孤儿记录收敛（routing.go
//     dropOrphanRecords）是请求时点单次判定，无二次确认——GetNode 确定性
//     miss 即删，依赖 90s TTL 保证误判窗口足够窄；DrainForDeployment 对
//     他节点实例只置 draining 标记、不代杀（killInstance 跳过他节点），
//     对端收敛节奏（busy 实例等在途请求结束后 ≤30s+reaper 间隔）可能慢于
//     本节点宽限，属接受的最终一致。

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
	// SaveNodeCapacity 写本节点常驻容量键（四期 4c M4）：SET
	// torchwood:fncap:resident:<node_id> = resident（TTL 与心跳同宽）。
	// 幂等覆写语义——值 = 写入方进程内权威计数（SET 实际值而非增量运算，
	// 失败重试/写丢失不产生漂移）。
	SaveNodeCapacity(ctx context.Context, nodeID string, resident int, ttl time.Duration) error
	// SumNodeCapacity 汇总全部节点的常驻容量键（SCAN + GET 求和，M4 全局
	// 上限的读取面）：SCAN 与 GET 之间过期的键按 0 计（死节点自然退出求和）。
	// 错误非 nil = Redis 不可用，调用方 fail-open（容量门退化为仅本节点上限）。
	SumNodeCapacity(ctx context.Context) (int, error)
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

// SaveNodeCapacity 写本节点容量键（M4；SET + TTL）。值 = 调用方进程内权威
// 计数（负值防御性钳 0——SET 实际值语义下不应出现，出现即调用方计数已错）。
func (r *redisRegistry) SaveNodeCapacity(ctx context.Context, nodeID string, resident int, ttl time.Duration) error {
	if resident < 0 {
		resident = 0
	}
	return r.rdb.Set(ctx, nodeCapacityKeyPrefix+nodeID, strconv.Itoa(resident), ttl).Err()
}

func (r *redisRegistry) SumNodeCapacity(ctx context.Context) (int, error) {
	total := 0
	var cursor uint64
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, nodeCapacityKeyPrefix+"*", 100).Result()
		if err != nil {
			return 0, fmt.Errorf("scan node capacity keys: %w", err)
		}
		for _, k := range keys {
			raw, err := r.rdb.Get(ctx, k).Result()
			if err != nil {
				continue // SCAN 与 GET 之间过期的键：按 0 计（死节点自然退出求和）
			}
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				continue // 半写/损坏值跳过，不得毒化整个求和
			}
			if n > 0 {
				total += n
			}
		}
		if next == 0 {
			return total, nil
		}
		cursor = next
	}
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
