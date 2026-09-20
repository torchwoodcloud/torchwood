package leaderboards

import (
	"context"
	"errors"

	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SubmitCommand 是提交入参（subject 由调用面决定：server 面显式、client 面
// 从 session 派生——client 面不存在代提交这回事，是结构性保证）。
type SubmitCommand struct {
	BoardID   string
	SubjectID string
	Value     int64
	Tiebreak  *int64
	Period    string
	RequestID string
}

// Submit 是 server 面提交：任意 subject（函数 / 运营 / 自动化）。
func (a *Leaderboards) Submit(ctx context.Context, cmd SubmitCommand) (*domainleaderboards.Snapshot, bool, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, false, err
	}
	return a.submitWithIdempotency(ctx, projectID, cmd)
}

// SubmitSelf 是 client 面提交：subject = 当前终端用户，board 必须开启
// client_submit。
func (a *Leaderboards) SubmitSelf(ctx context.Context, cmd SubmitCommand) (*domainleaderboards.Snapshot, bool, error) {
	projectID, userID, err := endUser(ctx)
	if err != nil {
		return nil, false, err
	}
	cmd.SubjectID = userID
	b, err := a.loadBoard(ctx, projectID, cmd.BoardID)
	if err != nil {
		return nil, false, mapLeaderboardError(err)
	}
	if !b.ClientSubmit {
		return nil, false, mapLeaderboardError(domainleaderboards.ErrClientSubmitDisabled)
	}
	return a.submitWithIdempotency(ctx, projectID, cmd)
}

// maxSubmitAttempts 是同键并发首插的整体补偿重试上限（每次重开事务；
// 唯一约束仲裁最多一个胜者，败者下一轮必走合并路径，2 次即收敛——3 留裕量）。
const maxSubmitAttempts = 3

func (a *Leaderboards) doSubmit(ctx context.Context, projectID string, cmd SubmitCommand) (*domainleaderboards.Snapshot, error) {
	b, err := a.loadBoard(ctx, projectID, cmd.BoardID)
	if err != nil {
		return nil, err
	}
	period, err := domainleaderboards.ResolveSubmittablePeriod(b, cmd.Period, a.ts())
	if err != nil {
		return nil, err
	}
	if err := b.ValidateValue(cmd.Value); err != nil {
		return nil, err
	}
	if err := validateSubjectID(cmd.SubjectID); err != nil {
		return nil, err
	}
	if b.HasTiebreak() != (cmd.Tiebreak != nil) {
		return nil, domainleaderboards.ErrTiebreakMismatch
	}

	for attempt := 0; ; attempt++ {
		snap, err := a.submitOnce(ctx, b, period, cmd)
		if err == nil {
			return snap, nil
		}
		// 同键并发首插的唯一约束败者：PG 中语句错误后本事务已 aborted，
		// 事务内重试必然 25P02——补偿必须整体重开事务（胜者已提交，新事务
		// GetForUpdate 即见其行、走合并路径）。其余错误原样返回。
		if errors.Is(err, domainleaderboards.ErrConcurrentInsert) && attempt+1 < maxSubmitAttempts {
			continue
		}
		if errors.Is(err, domainleaderboards.ErrConcurrentInsert) {
			return nil, status.Error(codes.Aborted, "leaderboards: concurrent submit conflict, retry")
		}
		return nil, err
	}
}

// submitOnce 单事务提交：行锁读取 → 合并/首插 →（Stats 在同事务续查）。
// ErrConcurrentInsert 原样穿出（db.Run 回滚该事务），由 doSubmit 重开事务补偿。
func (a *Leaderboards) submitOnce(ctx context.Context, b *domainleaderboards.Board, periodKey string, cmd SubmitCommand) (*domainleaderboards.Snapshot, error) {
	var snap *domainleaderboards.Snapshot
	err := a.db.Run(ctx, func(ctx context.Context) error {
		e, err := a.submitInTx(ctx, b, periodKey, cmd)
		if err != nil {
			return err
		}
		stats, err := a.entries.Stats(ctx, b, periodKey, e)
		if err != nil {
			return err
		}
		snap = &domainleaderboards.Snapshot{BoardID: b.ID, PeriodKey: periodKey, Entry: e, Stats: stats}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// submitInTx 在同一事务内完成行锁读取 → 合并/首插。首插并发竞争经唯一约束
// 仲裁：败者以 ErrConcurrentInsert 返回并穿出事务（不要在事务内重读重试——
// 事务已 aborted），由 doSubmit 整体重开事务后走合并路径。
func (a *Leaderboards) submitInTx(ctx context.Context, b *domainleaderboards.Board, periodKey string, cmd SubmitCommand) (*domainleaderboards.Entry, error) {
	cur, err := a.entries.GetForUpdate(ctx, b.ProjectID, b.ID, periodKey, cmd.SubjectID)
	if err != nil {
		return nil, err
	}
	now := a.ts()
	if cur == nil {
		e := &domainleaderboards.Entry{
			ProjectID:     b.ProjectID,
			BoardID:       b.ID,
			PeriodKey:     periodKey,
			SubjectID:     cmd.SubjectID,
			Value:         cmd.Value,
			TiebreakValue: cmd.Tiebreak,
			SubmitCount:   1,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := a.entries.Insert(ctx, e); err != nil {
			return nil, err // ErrConcurrentInsert 穿出触发整体重试
		}
		return e, nil
	}
	// quota：每次受理的提交都计数（policy=best 的同分/更低分重放也占额度；
	// 网络层重试由 request_id 幂等去重，不重复烧额度）。
	if cur.SubmitCount >= b.PerSubjectLimit {
		return nil, domainleaderboards.ErrSubmitLimitExceeded
	}
	value, tb, improved := domainleaderboards.MergeSubmit(b, cur, cmd.Value, cmd.Tiebreak, now)
	cur.Value = value
	cur.TiebreakValue = tb
	cur.SubmitCount++
	if improved {
		cur.UpdatedAt = now
	}
	if err := a.entries.SaveMerged(ctx, cur); err != nil {
		return nil, err
	}
	return cur, nil
}

func validateSubjectID(s string) error {
	if s == "" {
		return domainleaderboards.ErrSubjectRequired
	}
	if len(s) > domainleaderboards.MaxSubjectIDLen {
		return domainleaderboards.ErrSubjectTooLong
	}
	return nil
}
