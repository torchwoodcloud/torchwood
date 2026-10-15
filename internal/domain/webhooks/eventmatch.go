// 订阅匹配器（functions-v4.md §6）：复用 eventmatch 内核
// （domain/functions/eventmatch.go 的解析与匹配语义），载体从 TriggerID
// 泛化为 WebhookID——**webhooks 侧独立索引**而非改造 functions.EventTriggerIndex
// （最小侵入：函数事件触发器面零改动，webhooks 自持快照）。
//
// 订阅串双形态与通配语义与函数事件触发器完全同口径：
//
//	databases.{database_id}.collections.{collection_id}.documents.{op}   文档写事件
//	{domain}.{...}[.*]                                                   系统行为事件
//
// 与 functions.EventTriggerIndex 的唯一语义差异：同一条事件命中同一
// webhook 的多条订阅串时**去重为一次投递**（函数侧多次投递由 event_id 幂等
// 吸收；webhook 是外部 HTTP 端点，重复投递无平台幂等兜底，订阅串数即
// 触发面的放大语义在此不成立——(webhook_id, event_id) 唯一索引亦同口径）。
package webhooks

import (
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

// subscription 是索引内的一条已展开订阅（webhook × 订阅串）。
type subscription struct {
	ProjectID string
	WebhookID string
	Pattern   domainfunctions.EventPattern
}

// SubscriptionIndex 是进程内订阅匹配器（文档：project → 订阅条目列表；
// 系统：project → 事件全名 → 订阅条目列表）：worker 周期快照扫描后整体
// 换入（atomic.Pointer），消费路径零查库（与函数事件触发器同款快照语义）。
type SubscriptionIndex struct {
	byProject       map[string][]subscription
	byProjectSystem map[string]map[string][]subscription
	count           int
}

// NewSubscriptionIndex 由订阅列表构建匹配器（忽略 paused 与不可解析的
// 订阅串——后者理论不可达：创建期已校验；防御坏数据不阻塞索引构建，
// 与 cron 领取跳过坏表达式行同款取舍）。系统形态订阅串按展开的事件全名
// 逐条登记。
func NewSubscriptionIndex(webhooks []Webhook) *SubscriptionIndex {
	idx := &SubscriptionIndex{
		byProject:       make(map[string][]subscription),
		byProjectSystem: make(map[string]map[string][]subscription),
	}
	for i := range webhooks {
		w := &webhooks[i]
		if !w.Enabled() {
			continue
		}
		for _, raw := range w.Events {
			p, err := domainfunctions.ParseEventPattern(raw)
			if err != nil {
				continue
			}
			sub := subscription{ProjectID: w.ProjectID, WebhookID: w.ID, Pattern: p}
			if p.Kind == domainfunctions.EventPatternKindSystem {
				m := idx.byProjectSystem[w.ProjectID]
				if m == nil {
					m = make(map[string][]subscription)
					idx.byProjectSystem[w.ProjectID] = m
				}
				for _, name := range p.SystemEvents {
					m[name] = append(m[name], sub)
					idx.count++
				}
				continue
			}
			idx.byProject[w.ProjectID] = append(idx.byProject[w.ProjectID], sub)
			idx.count++
		}
	}
	return idx
}

// Match 返回某项目内命中该文档写事件的全部 webhook id（同一 webhook 多条
// 订阅串命中同一事件 → 去重为一次投递，见包注释）；无命中返回 nil。
func (x *SubscriptionIndex) Match(projectID, databaseID, collectionID, op string) []string {
	if x == nil {
		return nil
	}
	subs := x.byProject[projectID]
	seen := make(map[string]struct{})
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		if s.ProjectID != projectID {
			continue
		}
		if !s.Pattern.Matches(databaseID, collectionID, op) {
			continue
		}
		if _, dup := seen[s.WebhookID]; dup {
			continue
		}
		seen[s.WebhookID] = struct{}{}
		out = append(out, s.WebhookID)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MatchSystem 返回某项目内订阅了该系统事件全名的全部 webhook id（去重
// 语义同 Match）；无命中返回 nil。
func (x *SubscriptionIndex) MatchSystem(projectID, event string) []string {
	if x == nil {
		return nil
	}
	m := x.byProjectSystem[projectID]
	if m == nil {
		return nil
	}
	subs := m[event]
	seen := make(map[string]struct{}, len(subs))
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		if _, dup := seen[s.WebhookID]; dup {
			continue
		}
		seen[s.WebhookID] = struct{}{}
		out = append(out, s.WebhookID)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Len 返回索引内的条目总数（系统形态按展开的事件全名逐条计；观测/日志用）。
func (x *SubscriptionIndex) Len() int {
	if x == nil {
		return 0
	}
	return x.count
}

// Empty 报告索引是否没有任何订阅。
func (x *SubscriptionIndex) Empty() bool { return x.Len() == 0 }
