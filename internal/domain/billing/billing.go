// Package billing 是 v3 平台用量计费子域（设计 §4）：计量点、小时 rollup、月账单文档。
package billing

import (
	"context"
	"encoding/json"
	"time"
)

// 计量指标名（数量一律 int64 最小单位：次数 / 字节 / 毫秒）。
const (
	MetricAPICalls           = "api_calls"
	MetricStorageBytes       = "storage_bytes"
	MetricFunctionDurationMS = "function_duration_ms"
	// MetricAnalyticsEvents 是 analytics 摄入计量点（docs/design/analytics.md
	// §4.3 / D15）：摄入用例按 accepted 条数 Incr，自动进 rollups → 账单。
	MetricAnalyticsEvents = "analytics_events"
)

// AllMetrics 是本期计量点全集（live 小时合并等枚举消费点的单一来源）。
var AllMetrics = []string{
	MetricAPICalls,
	MetricStorageBytes,
	MetricFunctionDurationMS,
	MetricAnalyticsEvents,
}

// BucketTTL 是 Redis 小时 bucket 的键 TTL：worker 停机后仍能扫到上一完整小时
// （验收：停机 1 小时重启 bucket 不丢）；设计要求 ≥ 48h。
const BucketTTL = 48 * time.Hour

// StatementStatus 是账单文档状态：当月 draft，月结束后 final。
type StatementStatus string

const (
	StatementDraft StatementStatus = "draft"
	StatementFinal StatementStatus = "final"
)

// KnownMetric 报告 metric 是否为本期计量点（不含 Realtime，D18）。
func KnownMetric(metric string) bool {
	switch metric {
	case MetricAPICalls, MetricStorageBytes, MetricFunctionDurationMS, MetricAnalyticsEvents:
		return true
	}
	return false
}

// HourBucket 把 t 截成 UTC 整点（小时 bucket 的 period_start）。
func HourBucket(t time.Time) time.Time {
	return t.UTC().Truncate(time.Hour)
}

// MonthBucket 把 t 截成 UTC 月初。
func MonthBucket(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// NextMonth 返回 UTC 下一月初。
func NextMonth(monthStart time.Time) time.Time {
	monthStart = MonthBucket(monthStart)
	return monthStart.AddDate(0, 1, 0)
}

// Bucket 是 Redis 中一个 (project, metric, hour) 计数。
type Bucket struct {
	ProjectID   string
	Metric      string
	PeriodStart time.Time
	Value       int64
}

// UsageCounter 是 Redis 小时计数端口（设计 §4.2：INCRBY (project, metric, hour_bucket)）。
type UsageCounter interface {
	// Incr 把 delta 加到当前小时 bucket（TTL ≥ 48h，仅首次写入设 TTL）。
	Incr(ctx context.Context, projectID, metric string, delta int64) error
	// IncrAt 加到指定小时 bucket（测试 / 补写）。
	IncrAt(ctx context.Context, projectID, metric string, hour time.Time, delta int64) error
	// Set 覆盖指定小时 bucket 的值（storage_bytes 是快照，走 SET 而非累加）。
	Set(ctx context.Context, projectID, metric string, hour time.Time, value int64) error
	// Get 读指定小时 bucket；键不存在返回 0, nil。
	Get(ctx context.Context, projectID, metric string, hour time.Time) (int64, error)
	// ListHour 扫出某完整小时的全部 bucket（worker rollup）。
	ListHour(ctx context.Context, hour time.Time) ([]Bucket, error)
}

// Rollup 是 usage_rollups 一行（小时聚合）。
type Rollup struct {
	ID          string
	ProjectID   string
	Metric      string
	PeriodStart time.Time
	Value       int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// StatementDetails 是账单 JSONB 明细：各 metric 的月合计（与 rollups SUM 对账）。
type StatementDetails struct {
	Metrics map[string]int64 `json:"metrics"`
	Hours   int              `json:"hours"`
}

// MarshalDetails 序列化账单明细；空 metrics 写成 {}。
func MarshalDetails(d StatementDetails) (json.RawMessage, error) {
	if d.Metrics == nil {
		d.Metrics = map[string]int64{}
	}
	return json.Marshal(d)
}

// UnmarshalDetails 反序列化账单明细。
func UnmarshalDetails(raw json.RawMessage) (StatementDetails, error) {
	var d StatementDetails
	if len(raw) == 0 || string(raw) == "null" {
		d.Metrics = map[string]int64{}
		return d, nil
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return StatementDetails{}, err
	}
	if d.Metrics == nil {
		d.Metrics = map[string]int64{}
	}
	return d, nil
}

// Statement 是 billing_statements 一行（月账单文档，一期不出票、不收款）。
type Statement struct {
	ID          string
	ProjectID   string
	PeriodStart time.Time
	PeriodEnd   time.Time
	Status      StatementStatus
	Details     StatementDetails
	CreatedAt   time.Time
	UpdatedAt   time.Time
	FinalizedAt *time.Time
}

// UsageRepo 持久化 usage_rollups。
type UsageRepo interface {
	// Upsert 按 (project_id, metric, period_start) 幂等写入：冲突时覆盖 value
	// （来源是 Redis 小时计数，重跑不累加）。
	Upsert(ctx context.Context, r *Rollup) error
	Get(ctx context.Context, projectID, metric string, periodStart time.Time) (*Rollup, error)
	// List 按项目 + 时间窗倒序列表（metric 空=全部）。
	List(ctx context.Context, projectID, metric string, from, to time.Time, limit int, before time.Time, beforeID string) ([]Rollup, error)
	// SumByMetric 返回时间窗内各 metric 的 SUM(value)（对账单据）。
	SumByMetric(ctx context.Context, projectID string, from, to time.Time) (map[string]int64, error)
	// DistinctHours 返回时间窗内 distinct period_start 数。
	DistinctHours(ctx context.Context, projectID string, from, to time.Time) (int, error)
}

// StatementRepo 持久化 billing_statements。
type StatementRepo interface {
	// Upsert 按 (project_id, period_start) 写入；已 final 的行不回退。
	Upsert(ctx context.Context, s *Statement) error
	Get(ctx context.Context, projectID string, periodStart time.Time) (*Statement, error)
	List(ctx context.Context, projectID string, limit int, before time.Time) ([]Statement, error)
}
