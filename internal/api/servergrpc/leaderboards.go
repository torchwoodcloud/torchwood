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

func (s *LeaderboardsService) GetLeaderboardSettlement(ctx context.Context, req *serverv1.GetLeaderboardSettlementRequest) (*serverv1.GetLeaderboardSettlementResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	settlement, grants, err := s.app.GetSettlement(ctx, req.GetBoardId(), req.GetPeriod())
	if err != nil {
		return nil, err
	}
	return &serverv1.GetLeaderboardSettlementResponse{
		Settlement: mapLeaderboardSettlement(settlement),
		Grants:     mapLeaderboardSettlementGrants(grants),
	}, nil
}

func (s *LeaderboardsService) ListLeaderboardSettlements(ctx context.Context, req *serverv1.ListLeaderboardSettlementsRequest) (*serverv1.ListLeaderboardSettlementsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	rows, err := s.app.ListSettlements(ctx, req.GetBoardId(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := make([]*sharedv1.LeaderboardSettlement, len(rows))
	for i := range rows {
		out[i] = mapLeaderboardSettlement(&rows[i])
	}
	return &serverv1.ListLeaderboardSettlementsResponse{Settlements: out}, nil
}

func (s *LeaderboardsService) CreateLeaderboardBoard(ctx context.Context, req *serverv1.CreateLeaderboardBoardRequest) (*sharedv1.LeaderboardBoard, error) {
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
	b, err := s.app.CreateBoardProvisioning(withAuditResource(ctx, req.GetId()), in)
	if err != nil {
		return nil, err
	}
	return mapLeaderboardBoard(b), nil
}

func (s *LeaderboardsService) GetLeaderboardBoard(ctx context.Context, req *serverv1.GetLeaderboardBoardRequest) (*sharedv1.LeaderboardBoard, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	b, err := s.app.GetBoard(ctx, req.GetBoardId())
	if err != nil {
		return nil, err
	}
	return mapLeaderboardBoard(b), nil
}

func (s *LeaderboardsService) ListLeaderboardBoards(ctx context.Context, req *serverv1.ListLeaderboardBoardsRequest) (*serverv1.ListLeaderboardBoardsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	boards, err := s.app.ListBoards(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*sharedv1.LeaderboardBoard, len(boards))
	for i := range boards {
		out[i] = mapLeaderboardBoard(&boards[i])
	}
	return &serverv1.ListLeaderboardBoardsResponse{Boards: out}, nil
}

func (s *LeaderboardsService) ListLeaderboardBoardPeriods(ctx context.Context, req *serverv1.ListLeaderboardBoardPeriodsRequest) (*serverv1.ListLeaderboardBoardPeriodsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	periods, err := s.app.ListPeriods(ctx, req.GetBoardId(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	return &serverv1.ListLeaderboardBoardPeriodsResponse{Periods: periods}, nil
}

func (s *LeaderboardsService) UpdateLeaderboardBoard(ctx context.Context, req *serverv1.UpdateLeaderboardBoardRequest) (*sharedv1.LeaderboardBoard, error) {
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
	b, err := s.app.UpdateBoard(withAuditResource(ctx, req.GetBoardId()), cmd)
	if err != nil {
		return nil, err
	}
	return mapLeaderboardBoard(b), nil
}

// mapLeaderboardLeaderboard 是 board 配置的 domain → proto 投影（与 console
// 面 mapConsoleBoard 同构；rewards 只读透出——读无风险，编辑权已收敛在
// console owner）。
func mapLeaderboardBoard(b *domainleaderboards.Board) *sharedv1.LeaderboardBoard {
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
		out.Rewards = make([]*sharedv1.LeaderboardRewardRule, len(b.Rewards))
		for i, r := range b.Rewards {
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
			out.Rewards[i] = rule
		}
	}
	return out
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

func mapLeaderboardSettlement(in *domainleaderboards.Settlement) *sharedv1.LeaderboardSettlement {
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
		Rules:      make([]*sharedv1.LeaderboardRewardRule, len(rules)),
	}
	if in.SettledAt != nil {
		out.SettledAt = timestamppb.New(*in.SettledAt)
	}
	for i, r := range rules {
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
		out.Rules[i] = rule
	}
	return out
}

func mapLeaderboardSettlementGrants(in []domainleaderboards.SettlementGrant) []*sharedv1.LeaderboardSettlementGrant {
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
