package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// LeaderboardsService 封装 Server API 的排行榜面（代任意 subject 提交 +
// 快照/榜读）。提交窗口 = {当前期, 上一期}，封榜后拒收。
type LeaderboardsService struct {
	c   *Client
	api serverv1.LeaderboardsServiceClient
}

func (s *LeaderboardsService) SubmitLeaderboardScore(ctx context.Context, req *serverv1.SubmitLeaderboardScoreRequest) (*sharedv1.LeaderboardScoreSnapshot, error) {
	return s.api.SubmitLeaderboardScore(ctx, req)
}

func (s *LeaderboardsService) GetLeaderboardEntry(ctx context.Context, boardID, subjectID, period string) (*sharedv1.LeaderboardScoreSnapshot, error) {
	return s.api.GetLeaderboardEntry(ctx, &serverv1.GetLeaderboardEntryRequest{BoardId: boardID, SubjectId: subjectID, Period: period})
}

func (s *LeaderboardsService) ListLeaderboardTop(ctx context.Context, req *serverv1.ListLeaderboardTopRequest) (*sharedv1.ListLeaderboardTopResponse, error) {
	return s.api.ListLeaderboardTop(ctx, req)
}

func (s *LeaderboardsService) GetLeaderboardSettlement(ctx context.Context, boardID, period string) (*serverv1.GetLeaderboardSettlementResponse, error) {
	return s.api.GetLeaderboardSettlement(ctx, &serverv1.GetLeaderboardSettlementRequest{BoardId: boardID, Period: period})
}

func (s *LeaderboardsService) ListLeaderboardSettlements(ctx context.Context, req *serverv1.ListLeaderboardSettlementsRequest) (*serverv1.ListLeaderboardSettlementsResponse, error) {
	return s.api.ListLeaderboardSettlements(ctx, req)
}
