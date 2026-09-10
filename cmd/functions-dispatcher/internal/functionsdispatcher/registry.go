package functionsdispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// InstanceRecord 是注册表成员：一个常驻实例的投影（instance_id/容器 IP/
// lease/busy 标记等；设计 §6 池策略）。
type InstanceRecord struct {
	InstanceID   string    `json:"instance_id"`
	ContainerID  string    `json:"container_id"`
	IP           string    `json:"ip"`
	DeploymentID string    `json:"deployment_id"`
	Busy         bool      `json:"busy"`
	Draining     bool      `json:"draining"`
	Requests     int64     `json:"requests"`
	SpawnedAt    time.Time `json:"spawned_at"`
	IdleSince    time.Time `json:"idle_since"`
	// LeaseUntil 是判活租约：dispatch 认领时续租；busy 实例不因租约过期
	// 被 reaper 误杀（超期过 stuckBusyGrace 视为请求方残留，强杀）。
	LeaseUntil time.Time `json:"lease_until"`
	// ——池策略落账（dispatcher 无 DB 依赖：spawn 时随记录固化，reaper
	// 依据其执行 idle 回收与保温保底）——
	MinInstances   int `json:"min_instances"`
	IdleTTLSeconds int `json:"idle_ttl_seconds"`
	MaxRequests    int `json:"max_requests"`
}

// FunctionRef 标识注册表中的一个函数池。
type FunctionRef struct {
	ProjectID  string
	FunctionID string
}

// Registry 是常驻实例注册表端口（Redis 实现；fake 测试用内存实现）。
type Registry interface {
	// List 返回函数池内全部实例记录。
	List(ctx context.Context, ref FunctionRef) ([]InstanceRecord, error)
	// ClaimIdle 原子认领一个空闲（busy=false 且 draining=false 且部署匹配）
	// 实例并置 busy + 续租；无可用实例返回 (nil, nil)。
	ClaimIdle(ctx context.Context, ref FunctionRef, deploymentID string, leaseUntil time.Time) (*InstanceRecord, error)
	// Save 写回/创建实例记录。
	Save(ctx context.Context, ref FunctionRef, rec InstanceRecord) error
	// Update 原子读改写单个实例（nowFn 由实现闭包决定）；rec 不存在返回 false。
	Update(ctx context.Context, ref FunctionRef, instanceID string, mutate func(*InstanceRecord)) (bool, error)
	// Delete 删除实例记录（幂等）。
	Delete(ctx context.Context, ref FunctionRef, instanceID string) error
	// ListFunctions 枚举当前持有实例的函数池（reaper 扫描入口）。
	ListFunctions(ctx context.Context) ([]FunctionRef, error)
	// AcquireSpawnLock 同函数并发 spawn 收敛锁（SETNX + TTL）：acquired=false
	// 表示已有并发 spawn 在途；release 幂等（锁持有者才删）。
	AcquireSpawnLock(ctx context.Context, ref FunctionRef, ttl time.Duration) (acquired bool, release func(), err error)
}

// redisRegistry 是 Registry 的 Redis 实现：
//   - 池键 torchwood:fninst:{project}:{function} 为 Hash（field=instance_id，
//     value=InstanceRecord JSON）；
//   - ClaimIdle 经 Lua 脚本原子求值（扫描 + 置 busy + 续租同事务）；
//   - spawn 键 torchwood:fnspawn:{project}:{function}（SET NX EX + 比对删除）。
type redisRegistry struct {
	rdb *redis.Client
}

// NewRedisRegistry 构造 Redis 注册表。
func NewRedisRegistry(rdb *redis.Client) Registry {
	return &redisRegistry{rdb: rdb}
}

func registryKey(ref FunctionRef) string {
	return registryKeyPrefix + ref.ProjectID + ":" + ref.FunctionID
}

func spawnLockKey(ref FunctionRef) string {
	return spawnLockPrefix + ref.ProjectID + ":" + ref.FunctionID
}

// claimIdleLua 在 Redis 侧原子完成「找空闲 → 置 busy → 续租」：一期串行
// 执行（1 并发/实例）依赖认领的互斥性，脚本失败即无实例返回。
var claimIdleLua = redis.NewScript(`
local vals = redis.call('HVALS', KEYS[1])
for i = 1, #vals do
  local ok, rec = pcall(cjson.decode, vals[i])
  if ok and type(rec) == 'table' then
    if rec.busy == false and rec.draining == false and rec.deployment_id == ARGV[1] then
      rec.busy = true
      rec.lease_until = tonumber(ARGV[2])
      redis.call('HSET', KEYS[1], rec.instance_id, cjson.encode(rec))
      return cjson.encode(rec)
    end
  end
end
return nil
`)

func (r *redisRegistry) List(ctx context.Context, ref FunctionRef) ([]InstanceRecord, error) {
	vals, err := r.rdb.HVals(ctx, registryKey(ref)).Result()
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	out := make([]InstanceRecord, 0, len(vals))
	for _, v := range vals {
		rec, err := decodeRecord(v)
		if err != nil {
			continue // 半写/损坏记录由 reaper 的幽灵对账兜底清理
		}
		out = append(out, *rec)
	}
	return out, nil
}

func (r *redisRegistry) ClaimIdle(ctx context.Context, ref FunctionRef, deploymentID string, leaseUntil time.Time) (*InstanceRecord, error) {
	raw, err := claimIdleLua.Run(ctx, r.rdb, []string{registryKey(ref)},
		deploymentID, leaseUntil.UnixMilli()).Text()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim idle instance: %w", err)
	}
	return decodeRecord(raw)
}

func (r *redisRegistry) Save(ctx context.Context, ref FunctionRef, rec InstanceRecord) error {
	raw, err := encodeRecord(rec)
	if err != nil {
		return err
	}
	return r.rdb.HSet(ctx, registryKey(ref), rec.InstanceID, raw).Err()
}

func (r *redisRegistry) Update(ctx context.Context, ref FunctionRef, instanceID string, mutate func(*InstanceRecord)) (bool, error) {
	key := registryKey(ref)
	raw, err := r.rdb.HGet(ctx, key, instanceID).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load instance: %w", err)
	}
	rec, err := decodeRecord(raw)
	if err != nil {
		return false, nil
	}
	mutate(rec)
	encoded, err := encodeRecord(*rec)
	if err != nil {
		return false, err
	}
	if err := r.rdb.HSet(ctx, key, instanceID, encoded).Err(); err != nil {
		return false, fmt.Errorf("save instance: %w", err)
	}
	return true, nil
}

func (r *redisRegistry) Delete(ctx context.Context, ref FunctionRef, instanceID string) error {
	return r.rdb.HDel(ctx, registryKey(ref), instanceID).Err()
}

func (r *redisRegistry) ListFunctions(ctx context.Context) ([]FunctionRef, error) {
	var refs []FunctionRef
	var cursor uint64
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, registryKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan instance registries: %w", err)
		}
		for _, k := range keys {
			rest := strings.TrimPrefix(k, registryKeyPrefix)
			project, function, ok := strings.Cut(rest, ":")
			if !ok || project == "" || function == "" {
				continue
			}
			refs = append(refs, FunctionRef{ProjectID: project, FunctionID: function})
		}
		if next == 0 {
			return refs, nil
		}
		cursor = next
	}
}

// acquireSpawnLockLua 比对删除（持锁者才删，防误删他人锁；与
// pkg/semaphore 的 release 同构）。
var acquireSpawnLockLua = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

func (r *redisRegistry) AcquireSpawnLock(ctx context.Context, ref FunctionRef, ttl time.Duration) (bool, func(), error) {
	token := newSpawnLockToken()
	key := spawnLockKey(ref)
	ok, err := r.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return false, nil, fmt.Errorf("acquire spawn lock: %w", err)
	}
	if !ok {
		return false, func() {}, nil
	}
	release := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = acquireSpawnLockLua.Run(ctx, r.rdb, []string{key}, token).Result()
	}
	return true, release, nil
}

func decodeRecord(raw string) (*InstanceRecord, error) {
	var rec InstanceRecord
	if err := jsonUnmarshal([]byte(raw), &rec); err != nil {
		return nil, err
	}
	if rec.InstanceID == "" {
		return nil, errors.New("instance record missing id")
	}
	return &rec, nil
}

func encodeRecord(rec InstanceRecord) (string, error) {
	b, err := jsonMarshal(rec)
	if err != nil {
		return "", fmt.Errorf("encode instance record: %w", err)
	}
	return string(b), nil
}
