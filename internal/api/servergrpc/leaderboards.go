package servergrpc

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appleaderboards "github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// LeaderboardsService 是排行榜 server 面 gRPC handler（薄：scope 在拦截器，
// 期窗口/合并/quota 在 use-case）。
type LeaderboardsService struct {
	serverv1.UnimplementedLeaderboardsServiceServer
	app *appleaderboards.Leaderboards
}

// NewLeaderboardsService constructs the server leaderboards service.
func NewLeaderboardsService(app *appleaderboards.Leaderboards) *LeaderboardsService {
	return &LeaderboardsService{app: app}
}

func (s *LeaderboardsService) SubmitLeaderboardScore(ctx context.Context, req *serverv1.SubmitLeaderboardScoreRequest) (*sharedv1.LeaderboardScoreSnapshot, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	snap, _, err := s.app.Submit(withAuditResource(ctx, req.GetBoardId()), appleaderboards.SubmitCommand{
		BoardID:   req.GetBoardId(),
		SubjectID: req.GetSubjectId(),
		Value:     req.GetValue(),
		Tiebreak:  req.TiebreakValue,
		Period:    req.GetPeriod(),
		RequestID: req.GetRequestId(),
	})
	if err != nil {
		return nil, err
	}
	return mapLeaderboardSnapshot(snap), nil
}

func (s *LeaderboardsService) GetLeaderboardEntry(ctx context.Context, req *serverv1.GetLeaderboardEntryRequest) (*sharedv1.LeaderboardScoreSnapshot, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	snap, err := s.app.GetEntry(ctx, req.GetBoardId(), req.GetSubjectId(), req.GetPeriod())
	if err != nil {
		return nil, err
	}
	return mapLeaderboardSnapshot(snap), nil
}

func (s *LeaderboardsService) ListLeaderboardTop(ctx context.Context, req *serverv1.ListLeaderboardTopRequest) (*sharedv1.ListLeaderboardTopResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	res, err := s.app.ListTop(ctx, req.GetBoardId(), req.GetPeriod(), int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return mapLeaderboardTop(res), nil
}

// mapLeaderboardDomainSnapshot / mapLeaderboardDomainEntry / mapLeaderboardDomainTop
// 是 domain → proto 的共享映射（三面 handler 各自复制一份同构实现，跟随
// assets/clientgrpc 的既有做法；不抽公共包避免 handler 层横向依赖）。
func mapLeaderboardSnapshot(in *domainleaderboards.Snapshot) *sharedv1.LeaderboardScoreSnapshot {
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
		out.Entry = mapLeaderboardEntry(in.Entry)
	}
	return out
}

func mapLeaderboardEntry(e *domainleaderboards.Entry) *sharedv1.LeaderboardEntry {
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

func mapLeaderboardTop(res *appleaderboards.TopResult) *sharedv1.ListLeaderboardTopResponse {
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
