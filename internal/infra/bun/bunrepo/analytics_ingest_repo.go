package bunrepo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
)

// analytics 表名（迁移 000019）；表名经 ModelTableExpr 以 schema 限定。
// rollup 三表名（analytics_query_repo 消费）一并登记于此。
const (
	analyticsEventsTable           = "analytics_events"
	analyticsEventDefinitionsTable = "analytics_event_definitions"
	analyticsDailyTable            = "analytics_daily"
	analyticsUserDaysTable         = "analytics_user_days"
	analyticsUserFirstSeenTable    = "analytics_user_first_seen"
	analyticsUserDeletionsTable    = "analytics_user_deletions"
)

var _ analytics.IngestRepository = (*AnalyticsIngestRepository)(nil)

// AnalyticsIngestRepository 是 analytics.IngestRepository 的 bun 实现
// （Scoped 模式，docs/design/analytics.md §5；D2：同步直写，不经队列）。
type AnalyticsIngestRepository struct {
	db *clients.Database
}

// NewAnalyticsIngestRepository 构造摄入仓储。
func NewAnalyticsIngestRepository(db *clients.Database) *AnalyticsIngestRepository {
	return &AnalyticsIngestRepository{db: db}
}

// analyticsEventInsertColumns 是 analytics_events 的 INSERT 列白名单：
// 排除 BIGSERIAL 调试序 id（库分配，bun 无 serial 特判——沿
// DocumentEventsOutbox.Seq 的白名单纪律）。
var analyticsEventInsertColumns = []string{
	"name", "user_id", "session_id", "source", "platform", "app_version",
	"occurred_at", "ingested_at", "props",
}

// InsertEvents 批量多行单语句 INSERT（bun 对 slice 模型渲染单条多 VALUES
// 语句；追加只写，at-least-once 不去重 D14）。SQL 构造在
// insertAnalyticsEvents（SQL 形状护栏测试的锚点）。
func (r *AnalyticsIngestRepository) InsertEvents(ctx context.Context, projectID string, events []analytics.Event) error {
	if len(events) == 0 {
		return nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, analyticsEventsTable, "ae")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	rows := make([]model.AnalyticsEvent, 0, len(events))
	for _, e := range events {
		ingested := e.IngestedAt
		if ingested.IsZero() {
			ingested = now
		}
		rows = append(rows, model.AnalyticsEvent{
			Name:       e.Name,
			UserID:     e.UserID,
			SessionID:  e.SessionID,
			Source:     e.Source,
			Platform:   e.Platform,
			AppVersion: e.AppVersion,
			OccurredAt: e.OccurredAt.UTC(),
			IngestedAt: ingested,
			Props:      analyticsPropsOrDefault(e.Props),
		})
	}
	return insertAnalyticsEvents(ctx, conn, expr, sch, rows)
}

// UpsertEventDefinitions 字典 upsert（D12：发现即时）：新名插入 first/last_seen；
// 存量名仅推进 last_seen（GREATEST），first_seen 不回退（conflict 目标是
// name 主键，未进 SET 的列保持原值）。批内同名防御性合并——同一条多行
// INSERT 语句两行命中同一冲突行会报 "cannot affect row a second time"。
// SQL 构造在 upsertAnalyticsEventDefinitions（形状护栏测试的锚点）。
func (r *AnalyticsIngestRepository) UpsertEventDefinitions(ctx context.Context, projectID string, defs []analytics.EventDefinition) error {
	if len(defs) == 0 {
		return nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, analyticsEventDefinitionsTable, "aed")
	if err != nil {
		return err
	}
	return upsertAnalyticsEventDefinitions(ctx, conn, expr, sch, mergeEventDefinitions(defs))
}

// CountEventDefinitions 项目内事件名总数（软上限判定量）。
func (r *AnalyticsIngestRepository) CountEventDefinitions(ctx context.Context, projectID string) (int64, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, analyticsEventDefinitionsTable, "aed")
	if err != nil {
		return 0, err
	}
	return countAnalyticsEventDefinitions(ctx, conn, expr, sch)
}

// ListEventDefinitionNames 项目内全部事件名（按名排序；软上限已达时区分
// 存量名/新名，D12）。SQL 构造在 listAnalyticsEventDefinitionNames（形状
// 护栏测试的锚点）。
func (r *AnalyticsIngestRepository) ListEventDefinitionNames(ctx context.Context, projectID string) ([]string, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, analyticsEventDefinitionsTable, "aed")
	if err != nil {
		return nil, err
	}
	return listAnalyticsEventDefinitionNames(ctx, conn, expr, sch)
}

func insertAnalyticsEvents(ctx context.Context, conn bun.IDB, expr string, sch bun.Ident, rows []model.AnalyticsEvent) error {
	_, err := conn.NewInsert().Model(&rows).ModelTableExpr(expr, sch).
		Column(analyticsEventInsertColumns...).
		Exec(ctx)
	return err
}

func upsertAnalyticsEventDefinitions(ctx context.Context, conn bun.IDB, expr string, sch bun.Ident, rows []model.AnalyticsEventDefinition) error {
	_, err := conn.NewInsert().Model(&rows).ModelTableExpr(expr, sch).
		Column("name", "first_seen", "last_seen").
		On("CONFLICT (name) DO UPDATE").
		Set("last_seen = GREATEST(aed.last_seen, EXCLUDED.last_seen)").
		Exec(ctx)
	return err
}

func countAnalyticsEventDefinitions(ctx context.Context, conn bun.IDB, expr string, sch bun.Ident) (int64, error) {
	n, err := conn.NewSelect().
		Model((*model.AnalyticsEventDefinition)(nil)).
		ModelTableExpr(expr, sch).
		Count(ctx)
	if err != nil {
		return 0, err
	}
	return int64(n), nil
}

func listAnalyticsEventDefinitionNames(ctx context.Context, conn bun.IDB, expr string, sch bun.Ident) ([]string, error) {
	var names []string
	err := conn.NewSelect().
		Model((*model.AnalyticsEventDefinition)(nil)).
		ModelTableExpr(expr, sch).
		Column("name").
		Order("name ASC").
		Scan(ctx, &names)
	if err != nil {
		return nil, err
	}
	return names, nil
}

// mergeEventDefinitions 按名去重：first_seen 取最早、last_seen 取最晚
// （保持首次出现顺序，渲染顺序确定）。
func mergeEventDefinitions(defs []analytics.EventDefinition) []model.AnalyticsEventDefinition {
	type bound struct {
		first time.Time
		last  time.Time
	}
	merged := make(map[string]*bound, len(defs))
	order := make([]string, 0, len(defs))
	for _, d := range defs {
		b, ok := merged[d.Name]
		if !ok {
			merged[d.Name] = &bound{first: d.FirstSeen, last: d.LastSeen}
			order = append(order, d.Name)
			continue
		}
		if d.FirstSeen.Before(b.first) {
			b.first = d.FirstSeen
		}
		if d.LastSeen.After(b.last) {
			b.last = d.LastSeen
		}
	}
	rows := make([]model.AnalyticsEventDefinition, 0, len(order))
	for _, name := range order {
		b := merged[name]
		rows = append(rows, model.AnalyticsEventDefinition{
			Name:      name,
			FirstSeen: b.first.UTC(),
			LastSeen:  b.last.UTC(),
		})
	}
	return rows
}

// analyticsPropsOrDefault 归一 props：空/NULL 落 '{}'（NOT NULL 列默认形态
// 与迁移 DDL 一致）。
func analyticsPropsOrDefault(props json.RawMessage) json.RawMessage {
	if len(props) == 0 || string(props) == "null" {
		return append(json.RawMessage(nil), jsonEmptyObject...)
	}
	return props
}
