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
	if len(req.GetRewards()) > 0 {
		in.Rewards = mapConsoleRewardRules(req.GetRewards())
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
	cmd.ClearRewards = req.GetClearRewards()
	if len(req.GetRewards()) > 0 {
		rules := mapConsoleRewardRules(req.GetRewards())
		cmd.Rewards = &rules
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

func (s *LeaderboardsService) GetLeaderboardSettlement(ctx context.Context, req *consolev1.GetLeaderboardSettlementRequest) (*consolev1.GetLeaderboardSettlementResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	settlement, grants, err := s.app.GetSettlement(ctx, req.GetBoardId(), req.GetPeriod())
	if err != nil {
		return nil, err
	}
	return &consolev1.GetLeaderboardSettlementResponse{
		Settlement: mapConsoleSettlement(settlement),
		Grants:     mapConsoleSettlementGrants(grants),
	}, nil
}

func (s *LeaderboardsService) ListLeaderboardSettlements(ctx context.Context, req *consolev1.ListLeaderboardSettlementsRequest) (*consolev1.ListLeaderboardSettlementsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	rows, err := s.app.ListSettlements(ctx, req.GetBoardId(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := make([]*sharedv1.LeaderboardSettlement, len(rows))
	for i := range rows {
		out[i] = mapConsoleSettlement(&rows[i])
	}
	return &consolev1.ListLeaderboardSettlementsResponse{Settlements: out}, nil
}

func (s *LeaderboardsService) VoidLeaderboardSettlement(ctx context.Context, req *consolev1.VoidLeaderboardSettlementRequest) (*consolev1.LeaderboardSettlementAck, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	if err := s.app.VoidSettlement(ctx, req.GetBoardId(), req.GetPeriod()); err != nil {
		return nil, err
	}
	return &consolev1.LeaderboardSettlementAck{}, nil
}

func (s *LeaderboardsService) RerunLeaderboardSettlement(ctx context.Context, req *consolev1.RerunLeaderboardSettlementRequest) (*consolev1.LeaderboardSettlementAck, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	if err := s.app.RerunSettlement(ctx, req.GetBoardId(), req.GetPeriod()); err != nil {
		return nil, err
	}
	return &consolev1.LeaderboardSettlementAck{}, nil
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
	if len(b.Rewards) > 0 {
		out.Rewards = mapConsoleRewardRuleProtos(b.Rewards)
	}
	return out
}

func mapConsoleRewardRules(in []*sharedv1.LeaderboardRewardRule) []domainleaderboards.RewardRule {
	out := make([]domainleaderboards.RewardRule, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		rule := domainleaderboards.RewardRule{
			AssetCode: r.GetAssetCode(),
			Amount:    r.GetAmount(),
		}
		if r.RankMin != nil {
			rule.RankMin = r.RankMin
		}
		if r.RankMax != nil {
			rule.RankMax = r.RankMax
		}
		if r.ValueMin != nil {
			rule.ValueMin = r.ValueMin
		}
		out = append(out, rule)
	}
	return out
}

func mapConsoleRewardRuleProtos(in []domainleaderboards.RewardRule) []*sharedv1.LeaderboardRewardRule {
	out := make([]*sharedv1.LeaderboardRewardRule, 0, len(in))
	for _, r := range in {
		rule := &sharedv1.LeaderboardRewardRule{
			AssetCode: r.AssetCode,
			Amount:    r.Amount,
		}
		if r.RankMin != nil {
			rule.RankMin = r.RankMin
		}
		if r.RankMax != nil {
			rule.RankMax = r.RankMax
		}
		if r.ValueMin != nil {
			rule.ValueMin = r.ValueMin
		}
		out = append(out, rule)
	}
	return out
}

func mapConsoleSettlement(in *domainleaderboards.Settlement) *sharedv1.LeaderboardSettlement {
	if in == nil {
		return nil
	}
	rules, _ := domainleaderboards.UnmarshalRewardRules(in.RulesSnapshot)
	out := &sharedv1.LeaderboardSettlement{
		BoardId:    in.BoardID,
		Period:     in.PeriodKey,
		Status:     in.Status,
		SealedAt:   timestamppb.New(in.SealedAt),
		EntryCount: in.EntryCount,
		GrantCount: in.GrantCount,
		Error:      in.Error,
		CreatedAt:  timestamppb.New(in.CreatedAt),
		UpdatedAt:  timestamppb.New(in.UpdatedAt),
		Rules:      mapConsoleRewardRuleProtos(rules),
	}
	if in.SettledAt != nil {
		out.SettledAt = timestamppb.New(*in.SettledAt)
	}
	return out
}

func mapConsoleSettlementGrants(in []domainleaderboards.SettlementGrant) []*sharedv1.LeaderboardSettlementGrant {
	out := make([]*sharedv1.LeaderboardSettlementGrant, len(in))
	for i := range in {
		out[i] = &sharedv1.LeaderboardSettlementGrant{
			RuleIndex:      in[i].RuleIndex,
			SubjectId:      in[i].SubjectID,
			AssetCode:      in[i].AssetCode,
			Amount:         in[i].Amount,
			IdempotencyKey: in[i].IdempotencyKey,
			Status:         in[i].Status,
			Error:          in[i].Error,
		}
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
