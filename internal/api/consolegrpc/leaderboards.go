package consolegrpc

import (
	"context"

	consolev1 "github.com/torchwoodcloud/torchwood/genproto/console/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appleaderboards "github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// LeaderboardsService 是排行榜 console 面 gRPC handler（薄）：board 配置
// CRUD、期 top、按 subject 查条目、删条目。
type LeaderboardsService struct {
	consolev1.UnimplementedLeaderboardsServiceServer
	app *appleaderboards.Leaderboards
}

// NewLeaderboardsService constructs the console leaderboards service.
func NewLeaderboardsService(app *appleaderboards.Leaderboards) *LeaderboardsService {
	return &LeaderboardsService{app: app}
}

func (s *LeaderboardsService) CreateLeaderboardBoard(ctx context.Context, req *consolev1.CreateLeaderboardBoardRequest) (*sharedv1.LeaderboardBoard, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	in := &domainleaderboards.Board{
		ID:               req.GetId(),
		Sort:             domainleaderboards.SortDirection(req.GetSort()),
		TieBreak:         domainleaderboards.TieBreak(req.GetTieBreak()),
		PeriodKind:       domainleaderboards.PeriodKind(req.GetPeriodKind()),
		PeriodTZ:         req.GetPeriodTz(),
		Policy:           domainleaderboards.Policy(req.GetPolicy()),
		ValueMin:         req.ValueMin,
		ValueMax:         req.ValueMax,
		ClientSubmit:     req.GetClientSubmit(),
		PerSubjectLimit:  req.GetPerSubjectSubmitLimit(),
		RetentionPeriods: req.GetRetentionPeriods(),
		SubjectKind:      req.GetSubjectKind(),
	}
	if req.GetTiebreakOrder() != "" {
		v := domainleaderboards.SortDirection(req.GetTiebreakOrder())
		in.TiebreakOrder = &v
	}
	b, err := s.app.CreateBoard(ctx, in)
	if err != nil {
		return nil, err
	}
	return mapConsoleBoard(b), nil
}

func (s *LeaderboardsService) ListLeaderboardBoards(ctx context.Context, req *consolev1.ListLeaderboardBoardsRequest) (*consolev1.ListLeaderboardBoardsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	boards, err := s.app.ListBoards(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*sharedv1.LeaderboardBoard, len(boards))
	for i := range boards {
		out[i] = mapConsoleBoard(&boards[i])
	}
	return &consolev1.ListLeaderboardBoardsResponse{Boards: out}, nil
}

func (s *LeaderboardsService) GetLeaderboardBoard(ctx context.Context, req *consolev1.GetLeaderboardBoardRequest) (*sharedv1.LeaderboardBoard, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	b, err := s.app.GetBoard(ctx, req.GetBoardId())
	if err != nil {
		return nil, err
	}
	return mapConsoleBoard(b), nil
}

func (s *LeaderboardsService) UpdateLeaderboardBoard(ctx context.Context, req *consolev1.UpdateLeaderboardBoardRequest) (*sharedv1.LeaderboardBoard, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	cmd := appleaderboards.UpdateBoardCommand{BoardID: req.GetBoardId()}
	if req.Sort != nil {
		v := domainleaderboards.SortDirection(req.GetSort())
		cmd.Sort = &v
	}
	if req.TiebreakOrder != nil {
		v := domainleaderboards.SortDirection(req.GetTiebreakOrder())
		cmd.TiebreakOrder = &v
	}
	cmd.ClearTiebreak = req.GetClearTiebreak()
	if req.TieBreak != nil {
		v := domainleaderboards.TieBreak(req.GetTieBreak())
		cmd.TieBreak = &v
	}
	if req.PeriodKind != nil {
		v := domainleaderboards.PeriodKind(req.GetPeriodKind())
		cmd.PeriodKind = &v
	}
	if req.PeriodTz != nil {
		tz := req.GetPeriodTz()
		cmd.PeriodTZ = &tz
	}
	if req.Policy != nil {
		v := domainleaderboards.Policy(req.GetPolicy())
		cmd.Policy = &v
	}
	cmd.ValueMin = req.ValueMin
	cmd.ValueMax = req.ValueMax
	cmd.ClearValueBounds = req.GetClearValueBounds()
	cmd.ClientSubmit = req.ClientSubmit
	cmd.PerSubjectLimit = req.PerSubjectSubmitLimit
	cmd.RetentionPeriods = req.RetentionPeriods
	if req.SubjectKind != nil {
		kind := req.GetSubjectKind()
		cmd.SubjectKind = &kind
	}
	b, err := s.app.UpdateBoard(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapConsoleBoard(b), nil
}

func (s *LeaderboardsService) DeleteLeaderboardBoard(ctx context.Context, req *consolev1.DeleteLeaderboardBoardRequest) (*sharedv1.Empty, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	if err := s.app.DeleteBoard(ctx, req.GetBoardId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *LeaderboardsService) ListLeaderboardBoardPeriods(ctx context.Context, req *consolev1.ListLeaderboardBoardPeriodsRequest) (*consolev1.ListLeaderboardBoardPeriodsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	periods, err := s.app.ListPeriods(ctx, req.GetBoardId(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	return &consolev1.ListLeaderboardBoardPeriodsResponse{Periods: periods}, nil
}

func (s *LeaderboardsService) ListLeaderboardTop(ctx context.Context, req *consolev1.ListLeaderboardTopRequest) (*sharedv1.ListLeaderboardTopResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	res, err := s.app.ListTop(ctx, req.GetBoardId(), req.GetPeriod(), int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return mapConsoleLeaderboardTop(res), nil
}

func (s *LeaderboardsService) GetLeaderboardEntry(ctx context.Context, req *consolev1.GetLeaderboardEntryRequest) (*sharedv1.LeaderboardScoreSnapshot, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	snap, err := s.app.GetEntry(ctx, req.GetBoardId(), req.GetSubjectId(), req.GetPeriod())
	if err != nil {
		return nil, err
	}
	return mapConsoleLeaderboardSnapshot(snap), nil
}

func (s *LeaderboardsService) DeleteLeaderboardEntry(ctx context.Context, req *consolev1.DeleteLeaderboardEntryRequest) (*sharedv1.Empty, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	if err := s.app.DeleteEntry(ctx, req.GetBoardId(), req.GetPeriod(), req.GetSubjectId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func mapConsoleBoard(b *domainleaderboards.Board) *sharedv1.LeaderboardBoard {
	if b == nil {
		return nil
	}
	out := &sharedv1.LeaderboardBoard{
		Id:                    b.ID,
		Sort:                  string(b.Sort),
		TieBreak:              string(b.TieBreak),
		PeriodKind:            string(b.PeriodKind),
		PeriodTz:              b.PeriodTZ,
		Policy:                string(b.Policy),
		ClientSubmit:          b.ClientSubmit,
		PerSubjectSubmitLimit: b.PerSubjectLimit,
		RetentionPeriods:      b.RetentionPeriods,
		SubjectKind:           b.SubjectKind,
		CreatedAt:             timestamppb.New(b.CreatedAt),
		UpdatedAt:             timestamppb.New(b.UpdatedAt),
	}
	if b.TiebreakOrder != nil {
		out.TiebreakOrder = string(*b.TiebreakOrder)
	}
	if b.ValueMin != nil {
		out.ValueMin = b.ValueMin
	}
	if b.ValueMax != nil {
		out.ValueMax = b.ValueMax
	}
	return out
}

func mapConsoleLeaderboardSnapshot(in *domainleaderboards.Snapshot) *sharedv1.LeaderboardScoreSnapshot {
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
		out.Entry = &sharedv1.LeaderboardEntry{
			BoardId:     in.Entry.BoardID,
			Period:      in.Entry.PeriodKey,
			SubjectId:   in.Entry.SubjectID,
			Value:       in.Entry.Value,
			SubmitCount: in.Entry.SubmitCount,
			CreatedAt:   timestamppb.New(in.Entry.CreatedAt),
			UpdatedAt:   timestamppb.New(in.Entry.UpdatedAt),
		}
		if in.Entry.TiebreakValue != nil {
			out.Entry.TiebreakValue = in.Entry.TiebreakValue
		}
	}
	return out
}

func mapConsoleLeaderboardTop(res *appleaderboards.TopResult) *sharedv1.ListLeaderboardTopResponse {
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
