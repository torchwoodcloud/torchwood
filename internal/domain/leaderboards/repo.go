package leaderboards

import (
	"context"
	"time"
)

// BoardRepo 持久化 leaderboard_boards（项目数据面）。写方法可加入调用方
// uow.Run；实现可从 ctx 读取连接。
type BoardRepo interface {
	Insert(ctx context.Context, b *Board) error
	// Get 读取榜配置；不存在返回 (nil, nil)。
	Get(ctx context.Context, projectID, boardID string) (*Board, error)
	// Update 白名单写：只更新可变字段（sort/tiebreak/policy/period/tz 等
	// 不可变字段永不进白名单，见 bunrepo 实现）。
	Update(ctx context.Context, b *Board) error
	Delete(ctx context.Context, projectID, boardID string) error
	List(ctx context.Context, projectID string) ([]Board, error)
}

// EntryRepo 持久化 leaderboard_entries。写路径必须在调用方 uow.Run 内
// （行锁 + 统计同事务）；实现可从 ctx 读取连接。
type EntryRepo interface {
	// GetForUpdate 行锁读取（uow 内）；不存在返回 (nil, nil)。
	GetForUpdate(ctx context.Context, projectID, boardID, periodKey, subjectID string) (*Entry, error)
	// Get 无锁读取；不存在返回 (nil, nil)。
	Get(ctx context.Context, projectID, boardID, periodKey, subjectID string) (*Entry, error)
	Insert(ctx context.Context, e *Entry) error
	// SaveMerged 白名单写（value / tiebreak_value / submit_count / updated_at）。
	SaveMerged(ctx context.Context, e *Entry) error
	// Stats 单条目快照统计：count(*) FILTER 单次扫描。
	Stats(ctx context.Context, b *Board, periodKey string, e *Entry) (EntryStats, error)
	// ListTop 全序分页：RANK()（competition，按 value）与 ROW_NUMBER()
	// （position，按全序）窗口在 SQL 内计算。
	ListTop(ctx context.Context, b *Board, periodKey string, limit, offset int) ([]TopEntry, error)
	// CountTotal 是该期条目总数（top 响应与 me 无条目场景）。
	CountTotal(ctx context.Context, projectID, boardID, periodKey string) (int64, error)
	// CountInBoard 是榜内条目总数（board 配置不可变字段的判定）。
	CountInBoard(ctx context.Context, projectID, boardID string) (int64, error)
	Delete(ctx context.Context, projectID, boardID, periodKey, subjectID string) (bool, error)
	// ListPeriods 返回已有条目的期 key 降序（console 期下拉）。
	ListPeriods(ctx context.Context, projectID, boardID string, limit int) ([]string, error)
	// PruneOlderThan 删除 period_key < cutoff 的整期条目（retention 任务）。
	PruneOlderThan(ctx context.Context, projectID, boardID, cutoff string) (int64, error)
	// ListRewardWinners 返回单条奖励规则命中的条目（rank/position 窗口在
	// SQL 内计算；边界语义跟随 board.TieBreak：parallel → rank 含端点，
	// earliest/latest → position 截断）。
	ListRewardWinners(ctx context.Context, b *Board, periodKey string, rule RewardRule, limit int) ([]Entry, error)
	// ListSettlablePeriods 返回已封榜（period_key < previousKey）且尚无
	// 结算行的期（结算扫描入口；基于状态而非定时投放，停机自动补算）。
	ListSettlablePeriods(ctx context.Context, projectID, boardID, previousKey string, limit int) ([]string, error)
}

// Clock 供测试注入时间。
type Clock func() time.Time
