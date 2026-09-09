package functions

import (
	"context"
	"time"
)

// 触发器词表（迁移 000015 CHECK 约束同源；设计 §3）。
const (
	TriggerTypeHTTP = "http"
	TriggerTypeCron = "cron"

	// ResponseMode 是 HTTP 触发器的双响应模式（二轮复审：微信 SSV 回调 1s
	// 超时×重试 3 次，纯同步大概率全超时致事件丢失；async_ack 平台先 200）。
	ResponseModeSync     = "sync"
	ResponseModeAsyncAck = "async_ack"

	// HandshakeEcho 是 GET 握手回显（K4：平台对 GET 直接回
	// {"echostr": <query.echostr>}，不 invoke；回显不授予任何能力）。
	HandshakeEcho = "echo"

	// Misfire 是 cron 错过策略：默认 catch_up_once（纯跳过对「每日重置/
	// 赛季结算」危险——那一分钟宕机 = 当天不重置）；skip 同样推进但不补跑。
	MisfireSkip        = "skip"
	MisfireCatchUpOnce = "catch_up_once"

	// 触发器配置默认值与上限（设计 §3 三轮复核：ack_body 上限 1KB 防带宽
	// 放大；body 上限缺省 64KB、per-trigger 可配至 1MB）。
	DefaultHTTPBodyLimitBytes = 64 << 10         // 64KB
	MaxHTTPBodyLimitBytes     = 1 << 20          // 1MB
	MaxAckBodyBytes           = 1024             // 1KB
	MaxTriggerTokenBytes      = 128              // token 列长度护栏（实际 22 字符）
	CronMisfireGrace          = 90 * time.Second // 到期判定宽限（>扫描周期 1min）
)

// TriggerConfig 是触发器分类型配置（function_triggers.config JSONB 的领域
// 投影；token 不入 config——提为独立列支撑 UNIQUE 查找，迁移 000015）。
// http/cron 各用其中一段字段；json tag 即存储编码。
type TriggerConfig struct {
	// —— http ——
	ResponseMode   string `json:"response_mode,omitempty"`
	AckBody        string `json:"ack_body,omitempty"`
	Handshake      string `json:"handshake,omitempty"`
	BodyLimitBytes int    `json:"body_limit_bytes,omitempty"`

	// —— cron ——
	Expr    string `json:"expr,omitempty"`
	Misfire string `json:"misfire,omitempty"`
}

// EffectiveBodyLimit 返回生效的请求体上限（配置 0 = 平台缺省 64KB）。
func (c TriggerConfig) EffectiveBodyLimit() int {
	if c.BodyLimitBytes <= 0 {
		return DefaultHTTPBodyLimitBytes
	}
	return c.BodyLimitBytes
}

// Trigger 是函数触发器实体（HTTP webhook / cron 定时）。
type Trigger struct {
	ID         string
	ProjectID  string
	FunctionID string
	Type       string // http | cron
	Config     TriggerConfig
	// Token 是 http 触发器的公开调用凭证（128bit，URL 即鉴权）；cron 为空。
	Token string
	// Enabled=false 的触发器对外按 404 处理（不泄露存在性）；cron 不再领取。
	Enabled bool
	// NextRunAt 是 cron 专用：下一次计划执行时刻（UTC）；http 恒 nil。
	NextRunAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TriggerSource 返回执行记录 trigger_source 列的取值（执行记录即审计载体）：
// http:{trigger_id} / cron:{trigger_id}。
func (t *Trigger) TriggerSource() string {
	return t.Type + ":" + t.ID
}

// CronNextInput 是领取推进决策的输入（cron 解析在领域层）。
type CronNextInput struct {
	Due     time.Time // 行内到期时刻（原 next_run_at）
	Now     time.Time // 本次扫描时刻
	Expr    string    // 5 字段 UTC cron 表达式
	Misfire string    // skip | catch_up_once（空按 catch_up_once）
}

// CronNextPlan 是领取推进计划：Next 是 CAS 推进目标（next_run_at 直接推进，
// 宕机 N 周期只补 1 次）；Run 决定本次到期是否补跑入队（skip 的 misfire 行
// 只推进不补跑）。
type CronNextPlan struct {
	Next time.Time
	Run  bool
}

// CronNextFunc 由领域层提供推进决策（bun 适配器在领取循环中回调）。
type CronNextFunc func(in CronNextInput) (CronNextPlan, error)

// DefaultCronNext 是 CronNextFunc 的领域默认实现。推进不变量：**CAS 推进
// 目标必须严格大于 now**——否则下一轮扫描会立即重复领取（观测到的双领取
// 根因：高频表达式 + 扫描滞后 60–90s 时，Next(due) 可能已 ≤ now）。
//   - 到期在宽限内（due ≥ now-Grace，准时/近准时）：正常节拍补跑本次，
//     Next = due 之后的下一计划时刻；若该时刻也已过期（不会立刻重领的
//     不变量被破坏），Next 退到 now 之后的下一计划时刻——同样只补跑本次。
//   - 已错过（misfire）：Next = now 之后的下一计划时刻（一次推进跨过全部
//     错过周期）；Run = misfire != skip（catch_up_once 补跑一次 / skip 只
//     推进不补跑）。
func DefaultCronNext(in CronNextInput) (CronNextPlan, error) {
	sched, err := ParseCronExpr(in.Expr)
	if err != nil {
		return CronNextPlan{}, err
	}
	// 准时：到期时刻仍在扫描宽限窗口内 → 正常节拍（补跑本次）。
	if !in.Due.Before(in.Now.Add(-CronMisfireGrace)) {
		next, err := sched.Next(in.Due)
		if err != nil {
			return CronNextPlan{}, err
		}
		if next.After(in.Now) {
			return CronNextPlan{Next: next, Run: true}, nil
		}
		// 自然下一节拍已过期：仍补跑本次，但推进目标取 now 之后（不重领）。
		next, err = sched.Next(in.Now)
		if err != nil {
			return CronNextPlan{}, err
		}
		return CronNextPlan{Next: next, Run: true}, nil
	}
	// misfire：直接推进到 now 之后的下一计划时刻。
	next, err := sched.Next(in.Now)
	if err != nil {
		return CronNextPlan{}, err
	}
	return CronNextPlan{Next: next, Run: in.Misfire != MisfireSkip}, nil
}

// CronClaim 是一次原子领取成功的到期 cron 触发器（CAS 赢家才出现在返回值
// 中——多实例并发扫描同一到期行只有一条 ClaimDueCron 调用拿到它）。
type CronClaim struct {
	ProjectID  string
	FunctionID string
	TriggerID  string
	// ScheduledFor 是本次补跑对应的计划时刻（原到期值；入队 data 携带给
	// 函数做幂等键）。misfire=skip 的行被推进但不出现在领取结果中。
	ScheduledFor time.Time
}

// TriggerRepo 持久化函数触发器（迁移 000015）。方法均按 (project, function)
// 收窄——跨项目/跨库同名合法是本项目物理模型不变量。
type TriggerRepo interface {
	CreateTrigger(ctx context.Context, t *Trigger) error
	GetTrigger(ctx context.Context, projectID, functionID, triggerID string) (*Trigger, error)
	ListTriggers(ctx context.Context, projectID, functionID string) ([]Trigger, error)
	// UpdateTrigger 全量更新可变列（token/config/enabled/next_run_at）；
	// id/project_id/function_id/type/created_at 不可变。
	UpdateTrigger(ctx context.Context, t *Trigger) error
	DeleteTrigger(ctx context.Context, projectID, functionID, triggerID string) error
	// GetTriggerByToken 按 (project, token) 查找启用的 http 触发器（公开调用
	// 热路径；project 同时校验防跨项目探测——未命中一律按不存在处理）。
	GetTriggerByToken(ctx context.Context, projectID, token string) (*Trigger, error)

	// ClaimDueCron 原子领取某项目内到期的 cron 触发器：候选扫描（无锁）后
	// 逐条 CAS 推进 next_run_at（UPDATE ... WHERE next_run_at=旧值，判
	// rows=1）——多实例并发只有赢家；推进计划（含 misfire 语义）经 next 回调
	// 由领域层给出。**必须先 CAS 后入队**：先入队后 CAS 在多实例下会双入队，
	// 执行行 queued→building 的 CAS 防不了两条不同 execution。返回 CAS 赢家
	// 中 Run=true 的条目（skip 的 misfire 行被推进但不返回）。
	ClaimDueCron(ctx context.Context, projectID string, now time.Time, limit int, next CronNextFunc) ([]CronClaim, error)
}
