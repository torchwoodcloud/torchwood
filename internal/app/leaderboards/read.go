package leaderboards

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"

	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TopResult 是 top 列表结果。
type TopResult struct {
	BoardID       string
	PeriodKey     string
	Total         int64
	Entries       []domainleaderboards.TopEntry
	NextPageToken string
}

// GetEntry 是 server / console 面读取：任意 subject 的快照（含分位）。
func (a *Leaderboards) GetEntry(ctx context.Context, boardID, subjectID, period string) (*domainleaderboards.Snapshot, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateSubjectID(subjectID); err != nil {
		return nil, mapLeaderboardError(err)
	}
	return a.getEntry(ctx, projectID, boardID, subjectID, period)
}

// GetMyEntry 是 client 面读取：当前用户自己的快照；无条目时 Entry 为 nil、
// Total 仍返回（冷启动"已有 N 人参与"场景）。
func (a *Leaderboards) GetMyEntry(ctx context.Context, boardID, period string) (*domainleaderboards.Snapshot, error) {
	projectID, userID, err := endUser(ctx)
	if err != nil {
		return nil, err
	}
	return a.getEntry(ctx, projectID, boardID, userID, period)
}

func (a *Leaderboards) getEntry(ctx context.Context, projectID, boardID, subjectID, period string) (*domainleaderboards.Snapshot, error) {
	b, err := a.loadBoard(ctx, projectID, boardID)
	if err != nil {
		return nil, mapLeaderboardError(err)
	}
	// 读路径：任意保留期可读（分享卡复走历史期），缺省当前期。
	periodKey, err := domainleaderboards.ResolveReadPeriod(b, period, a.ts())
	if err != nil {
		return nil, mapLeaderboardError(err)
	}
	e, err := a.entries.Get(ctx, projectID, boardID, periodKey, subjectID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		total, err := a.entries.CountTotal(ctx, projectID, boardID, periodKey)
		if err != nil {
			return nil, err
		}
		return &domainleaderboards.Snapshot{
			BoardID:   b.ID,
			PeriodKey: periodKey,
			Stats:     domainleaderboards.EntryStats{Total: total},
		}, nil
	}
	stats, err := a.entries.Stats(ctx, b, periodKey, e)
	if err != nil {
		return nil, err
	}
	return &domainleaderboards.Snapshot{BoardID: b.ID, PeriodKey: periodKey, Entry: e, Stats: stats}, nil
}

// ListTop 是全序分页（rank = competition，position = 全序位次）。
func (a *Leaderboards) ListTop(ctx context.Context, boardID, period string, pageSize int, pageToken string) (*TopResult, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	b, err := a.loadBoard(ctx, projectID, boardID)
	if err != nil {
		return nil, mapLeaderboardError(err)
	}
	periodKey, err := domainleaderboards.ResolveReadPeriod(b, period, a.ts())
	if err != nil {
		return nil, mapLeaderboardError(err)
	}
	if pageSize <= 0 {
		pageSize = defaultTopPageSize
	}
	if pageSize > maxTopPageSize {
		pageSize = maxTopPageSize
	}
	offset, err := decodeTopToken(pageToken)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	entries, err := a.entries.ListTop(ctx, b, periodKey, pageSize+1, offset)
	if err != nil {
		return nil, err
	}
	total, err := a.entries.CountTotal(ctx, projectID, boardID, periodKey)
	if err != nil {
		return nil, err
	}
	res := &TopResult{BoardID: b.ID, PeriodKey: periodKey, Total: total}
	if len(entries) > pageSize {
		res.Entries = entries[:pageSize]
		res.NextPageToken = encodeTopToken(offset + pageSize)
	} else {
		res.Entries = entries
	}
	return res, nil
}

// ListPeriods 返回已有条目的期 key 降序（console 期下拉）。
func (a *Leaderboards) ListPeriods(ctx context.Context, boardID string, limit int) ([]string, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := a.loadBoard(ctx, projectID, boardID); err != nil {
		return nil, mapLeaderboardError(err)
	}
	return a.entries.ListPeriods(ctx, projectID, boardID, limit)
}

// DeleteEntry 是 console 删条目（作弊处理）。封榜即终局：只有仍在提交窗口
// {当前期, 上一期} 内的期可删，更早的期结果不可变。
func (a *Leaderboards) DeleteEntry(ctx context.Context, boardID, period, subjectID string) error {
	projectID, err := projectScope(ctx)
	if err != nil {
		return err
	}
	b, err := a.loadBoard(ctx, projectID, boardID)
	if err != nil {
		return mapLeaderboardError(err)
	}
	// 复用写路径的窗口判定：显式期必须 ∈ {当前期, 上一期}。
	periodKey, err := domainleaderboards.ResolveSubmittablePeriod(b, period, a.ts())
	if err != nil {
		return mapLeaderboardError(err)
	}
	if err := validateSubjectID(subjectID); err != nil {
		return mapLeaderboardError(err)
	}
	ok, err := a.entries.Delete(ctx, projectID, boardID, periodKey, subjectID)
	if err != nil {
		return err
	}
	if !ok {
		return mapLeaderboardError(domainleaderboards.ErrEntryNotFound)
	}
	return nil
}

// top 分页 token 编码 offset（全序确定，浅翻页下 offset 足够；keyset 游标
// 是将来 around 的接缝）。上限 maxTopOffset 防深翻页放大。
func encodeTopToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeTopToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, err
	}
	offset, err := strconv.Atoi(string(raw))
	if err != nil || offset < 0 || offset > maxTopOffset {
		return 0, fmt.Errorf("bad offset token")
	}
	return offset, nil
}
