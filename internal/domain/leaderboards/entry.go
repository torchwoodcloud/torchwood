package leaderboards

import "time"

// Entry 是排行榜条目：唯一键 (board, period, subject) 由表主键内建
// （去重是模型的一部分）。
type Entry struct {
	ProjectID     string
	BoardID       string
	PeriodKey     string
	SubjectID     string
	Value         int64
	TiebreakValue *int64
	SubmitCount   int32
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// EntryStats 是单条目快照统计（与写入同事务计算，一致性快照）。
type EntryStats struct {
	// Total 是该期条目总数。
	Total int64
	// Rank 是 competition rank（并列同名次，1 起）；无条目时为 0。
	Rank int64
	// Position 是全序位次（tie-break 决出，1 起）；无条目时为 0。
	Position int64
	// Below 是 value 严格劣于我的条目数（百分位分子，不受 tie-break 影响）。
	Below int64
}

// Snapshot 是 submit / me / getEntry 的统一响应载荷（无条目时 Entry 为 nil，
// Total 仍返回——冷启动"已有 N 人参与"场景）。
type Snapshot struct {
	BoardID   string
	PeriodKey string
	Entry     *Entry
	Stats     EntryStats
}

// TopEntry 是 top 列表行（RANK() 窗口在 SQL 内计算）。
type TopEntry struct {
	Entry
	Rank     int64
	Position int64
}
