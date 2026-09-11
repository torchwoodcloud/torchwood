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
