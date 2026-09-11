package clientgrpc

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appleaderboards "github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// LeaderboardsService 是终端用户排行榜面 gRPC handler（薄）。submit 的
// subject 恒从 session 派生——本面不存在代他人提交这回事。
type LeaderboardsService struct {
	clientv1.UnimplementedLeaderboardsServiceServer
	app *appleaderboards.Leaderboards
}

// NewLeaderboardsService constructs the client leaderboards service.
func NewLeaderboardsService(app *appleaderboards.Leaderboards) *LeaderboardsService {
	return &LeaderboardsService{app: app}
}

func (s *LeaderboardsService) SubmitLeaderboardScore(ctx context.Context, req *clientv1.SubmitLeaderboardScoreRequest) (*sharedv1.LeaderboardScoreSnapshot, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	snap, _, err := s.app.SubmitSelf(ctx, appleaderboards.SubmitCommand{
		BoardID:   req.GetBoardId(),
		Value:     req.GetValue(),
		Tiebreak:  req.TiebreakValue,
		Period:    req.GetPeriod(),
		RequestID: req.GetRequestId(),
	})
	if err != nil {
		return nil, err
	}
	return mapClientLeaderboardSnapshot(snap), nil
}

func (s *LeaderboardsService) GetMyLeaderboardEntry(ctx context.Context, req *clientv1.GetMyLeaderboardEntryRequest) (*sharedv1.LeaderboardScoreSnapshot, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	snap, err := s.app.GetMyEntry(ctx, req.GetBoardId(), req.GetPeriod())
	if err != nil {
		return nil, err
	}
	return mapClientLeaderboardSnapshot(snap), nil
}

func (s *LeaderboardsService) ListLeaderboardTop(ctx context.Context, req *clientv1.ListLeaderboardTopRequest) (*sharedv1.ListLeaderboardTopResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	res, err := s.app.ListTop(ctx, req.GetBoardId(), req.GetPeriod(), int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return mapClientLeaderboardTop(res), nil
}

func mapClientLeaderboardSnapshot(in *domainleaderboards.Snapshot) *sharedv1.LeaderboardScoreSnapshot {
	if in == nil {
		return nil
	}
	out := &sharedv1.LeaderboardScoreSnapshot{
		BoardId:  in.BoardID,
		Period:   in.PeriodKey,
		Total:    in.Stats.Total,
		Rank:     in.Stats.Rank,
		Position: in.Stats.Position,
		Below:    in.Stats.Below,
	}
	if in.Entry != nil {
		out.Entry = mapClientLeaderboardEntry(in.Entry)
	}
	return out
}

func mapClientLeaderboardEntry(e *domainleaderboards.Entry) *sharedv1.LeaderboardEntry {
	if e == nil {
		return nil
	}
	out := &sharedv1.LeaderboardEntry{
		BoardId:     e.BoardID,
		Period:      e.PeriodKey,
		SubjectId:   e.SubjectID,
		Value:       e.Value,
		SubmitCount: e.SubmitCount,
		CreatedAt:   timestamppb.New(e.CreatedAt),
		UpdatedAt:   timestamppb.New(e.UpdatedAt),
	}
	if e.TiebreakValue != nil {
		out.TiebreakValue = e.TiebreakValue
	}
	return out
}

func mapClientLeaderboardTop(res *appleaderboards.TopResult) *sharedv1.ListLeaderboardTopResponse {
	if res == nil {
		return nil
	}
	out := &sharedv1.ListLeaderboardTopResponse{
		Period:        res.PeriodKey,
		Total:         res.Total,
		NextPageToken: res.NextPageToken,
		Entries:       make([]*sharedv1.LeaderboardTopEntry, len(res.Entries)),
	}
	for i := range res.Entries {
		row := &sharedv1.LeaderboardTopEntry{
			SubjectId: res.Entries[i].SubjectID,
			Value:     res.Entries[i].Value,
			Rank:      res.Entries[i].Rank,
			Position:  res.Entries[i].Position,
			UpdatedAt: timestamppb.New(res.Entries[i].UpdatedAt),
		}
		if res.Entries[i].TiebreakValue != nil {
			row.TiebreakValue = res.Entries[i].TiebreakValue
		}
		out.Entries[i] = row
	}
	return out
}
