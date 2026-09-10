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
// lease/inflight 计数等；执行器 v2 设计 §6 池策略 + v3 注册表 inflight 化，
// docs/design/functions-v3.md §1.3）。
//
// 时间字段约定（不变量，v3 彻底版）：记录内所有时间字段一律数值 Unix 毫秒
// ——claimIdleLua/releaseInstanceLua 做 cjson 往返重写整条记录，time.Time
// 的 RFC3339 字符串一旦被 Lua 触碰即毒化记录（Go 侧 decode 必败 → 记录成
// 账外幽灵）；毫秒化后 Lua 可安全改写全部字段（v3 §1.3「时间字段毫秒化
// （排雷）」，cjson encode 整数无精度损失，毫秒时间戳 < 2^53）。
//
// 兼容性（v3 §1.3「兼容陷阱」，必须处理）：注册表只能从记录侧对账（清
// Redis 键升级会泄漏容器），旧记录（busy 布尔、spawned_at/idle_since
// RFC3339 字符串）经自定义 UnmarshalJSON 双读兼容——busy=true 推导
// inflight=1、时间字符串解析为毫秒；旧记录自然老化，无升级 runbook。
type InstanceRecord struct {
	InstanceID   string `json:"instance_id"`
	ContainerID  string `json:"container_id"`
	IP           string `json:"ip"`
	DeploymentID string `json:"deployment_id"`
	// Inflight 是在途请求数（v3：取代 Busy 布尔互斥，busy ≡ inflight > 0）。
	// concurrency=1（本切片恒定）时退化为串行互斥，对外语义与 v2 等价。
	Inflight int `json:"inflight"`
	// Concurrency 是实例并发上限：spawn 时从池策略固化进记录（v3 §1.1
	// 「生效时机」——实例终生按 spawn 时策略服务）；零值按 1 兜底（旧记录
	// 兼容，claim Lua 同款 fallback）。
	Concurrency int   `json:"concurrency"`
	Draining    bool  `json:"draining"`
	Requests    int64 `json:"requests"`
	// Timeouts 是累计超时计数（v3 §1.4 超时熔断）：超时释放路径累加、正常
	// 完成不重置；达熔断阈值（PoolConfig.TimeoutBudget，默认 5）杀实例重建
	// ——杀实例即删记录，计数随实例生命周期清零。
	Timeouts    int   `json:"timeouts"`
	SpawnedAtMS int64 `json:"spawned_at_ms"`
	IdleSinceMS int64 `json:"idle_since_ms"`
	// LeaseUntilMS 是判活租约（Unix 毫秒）：dispatch 认领与每次释放时续租；
	// 在途实例不因租约过期被 reaper 误杀（超期过 stuckBusyGrace 视为请求方
	// 残留，强杀）。数值毫秒形态供 Lua 原位改写。
	LeaseUntilMS int64 `json:"lease_until_ms"`
	// ——池策略落账（dispatcher 无 DB 依赖：spawn 时随记录固化，reaper
	// 依据其执行 idle 回收与保温保底；max_requests 判 draining 同样以记录
	// 固化值为准——释放 Lua 对抗审查修正，不用请求携带值，避免函数更新后
	// 策略与实例不一致的判定歧义，v3 §1.3）——
	MinInstances   int `json:"min_instances"`
	IdleTTLSeconds int `json:"idle_ttl_seconds"`
	MaxRequests    int `json:"max_requests"`
}

// UnmarshalJSON 双读兼容旧版记录（v3 §1.3「兼容陷阱」）：
//   - inflight 缺省时由 busy 布尔推导（busy=true → inflight=1）；
//   - spawned_at/idle_since 为 RFC3339 字符串时解析为毫秒（仅在新毫秒字段
//     缺位时生效）。
//
// 新记录按新字段名编码（Marshal 走默认 struct tag，旧字段不再写出）。
func (r *InstanceRecord) UnmarshalJSON(b []byte) error {
	type alias InstanceRecord // 防递归
	var a alias
	if err := jsonUnmarshal(b, &a); err != nil {
		return err
	}
	var legacy struct {
		Inflight  *int    `json:"inflight"`
		Busy      *bool   `json:"busy"`
		SpawnedAt *string `json:"spawned_at"`
		IdleSince *string `json:"idle_since"`
	}
	if err := jsonUnmarshal(b, &legacy); err != nil {
		return err
	}
	if legacy.Inflight == nil && legacy.Busy != nil && *legacy.Busy {
		a.Inflight = 1
	}
	legacyToMS := func(s *string, ms *int64) {
		if s == nil || *ms != 0 {
			return
		}
		if t, err := time.Parse(time.RFC3339, *s); err == nil {
			*ms = t.UnixMilli()
		}
	}
	legacyToMS(legacy.SpawnedAt, &a.SpawnedAtMS)
	legacyToMS(legacy.IdleSince, &a.IdleSinceMS)
	*r = InstanceRecord(a)
	return nil
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
	// ClaimIdle 原子认领一个可服务（inflight < concurrency 且 draining=false
	// 且部署匹配）实例并 inflight+1 + 续租；无可用实例返回 (nil, nil)。
	// （v3 §1.1 背压顺序①；concurrency=1 时与 v2 busy 互斥语义逐步等价。）
	ClaimIdle(ctx context.Context, ref FunctionRef, deploymentID string, leaseUntil time.Time) (*InstanceRecord, error)
	// Save 写回/创建实例记录。
	Save(ctx context.Context, ref FunctionRef, rec InstanceRecord) error
	// Release 原子释放一次在途请求（v3 §1.3 释放脚本——Go 读改写释放与
	// claim/release 并发交错会丢更新造成计数漂移〔永久「满载」或超卖〕，
	// 必须 Lua 原子）：inflight-1（下限 0）+ requests+1 + 续租；inflight==0
	// 时落 idle_since_ms 并按记录固化 max_requests 判 draining；
	// timedOut=true 时累加 timeouts（熔断计数，v3 §1.4）。返回更新后记录
	// （记录已消失返回 nil）。
	Release(ctx context.Context, ref FunctionRef, instanceID string, now time.Time, leaseUntil time.Time, timedOut bool) (*InstanceRecord, error)
	// Update 原子读改写单个实例（nowFn 由实现闭包决定）；rec 不存在返回 false。
	// （v3 起仅限管理面改写如 drain 标记；执行路径收账一律走 Release。）
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

// claimIdleLua 在 Redis 侧原子完成「找可服务实例 → inflight+1 → 续租」
// （v3 §1.3 认领脚本）：可服务 = inflight < concurrency 且未 draining 且
// 部署匹配。旧记录兼容（v3 §1.3）：inflight 缺省由 busy 推导、concurrency
// 缺省 1。记录时间字段已全部数值毫秒化，Lua cjson 往返不改写任何形态
// （v3 §1.3「时间字段毫秒化（排雷）」）。脚本失败即无实例返回。
var claimIdleLua = redis.NewScript(`
local vals = redis.call('HVALS', KEYS[1])
for i = 1, #vals do
  local ok, rec = pcall(cjson.decode, vals[i])
  if ok and type(rec) == 'table' then
    local inf = rec.inflight
    if inf == nil then inf = (rec.busy == true) and 1 or 0 end
    local c = rec.concurrency
    if c == nil or c <= 0 then c = 1 end
    if rec.draining == false and rec.deployment_id == ARGV[1] and inf < c then
      rec.inflight = inf + 1
      rec.lease_until_ms = tonumber(ARGV[2])
      redis.call('HSET', KEYS[1], rec.instance_id, cjson.encode(rec))
      return cjson.encode(rec)
    end
  end
end
return nil
`)

// releaseInstanceLua 在 Redis 侧原子完成「inflight-1 → requests+1 → 续租 →
// idle/draining 判定」（v3 §1.3 释放脚本，本切片最重要的正确性改动——
// 现状 Go 读改写释放非原子，串行期靠 busy 互斥免除竞态；inflight 化后同
// 实例 claim/release 并发交错，丢更新 = 计数漂移，必须 Lua 原子）。
//   - max_requests 判 draining 用记录固化值（对抗审查修正），不用请求携带
//     值，避免函数更新后策略与实例不一致的判定歧义；
//   - idle_since_ms 只在 inflight 归零（最后一个在途请求完成）时落；
//   - ARGV[4]='1'（超时释放路径）累加 timeouts 熔断计数（v3 §1.4）。
//
// now_ms 由 Go 传入，Lua 不取时钟。超时路径同样走本脚本（幂等）。
var releaseInstanceLua = redis.NewScript(`
local raw = redis.call('HGET', KEYS[1], ARGV[1])
if not raw then return nil end
local ok, rec = pcall(cjson.decode, raw)
if not ok then return nil end
local inf = rec.inflight
if inf == nil then inf = (rec.busy == true) and 1 or 0 end
rec.inflight = math.max(0, inf - 1)
rec.requests = (rec.requests or 0) + 1
rec.lease_until_ms = tonumber(ARGV[3])
if ARGV[4] == '1' then
  rec.timeouts = (rec.timeouts or 0) + 1
end
if rec.inflight == 0 then
  rec.idle_since_ms = tonumber(ARGV[2])
  if (rec.max_requests or 0) > 0 and rec.requests >= rec.max_requests then
    rec.draining = true
  end
end
redis.call('HSET', KEYS[1], rec.instance_id, cjson.encode(rec))
return cjson.encode(rec)
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

func (r *redisRegistry) Release(ctx context.Context, ref FunctionRef, instanceID string, now time.Time, leaseUntil time.Time, timedOut bool) (*InstanceRecord, error) {
	timeoutFlag := "0"
	if timedOut {
		timeoutFlag = "1"
	}
	raw, err := releaseInstanceLua.Run(ctx, r.rdb, []string{registryKey(ref)},
		instanceID, now.UnixMilli(), leaseUntil.UnixMilli(), timeoutFlag).Text()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("release instance: %w", err)
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
