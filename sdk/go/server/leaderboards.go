package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// LeaderboardsService 封装 Server API 的排行榜面（代任意 subject 提交 +
// 快照/榜读 + board 配置管控）。提交窗口 = {当前期, 上一期}，封榜后拒收；
// board 管控写动词需 leaderboards.admin（与 leaderboards.write 刻意分离）。
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

// —— board 配置管控（预置与门禁自动化；Create/Update 需 leaderboards.admin）——

// CreateLeaderboardBoard 幂等建榜：已存在且配置逐字段相等（缺省归一后）→
// 200 + 现状（重放安全）；不等 → ALREADY_EXISTS（错误消息附字段 diff）。
func (s *LeaderboardsService) CreateLeaderboardBoard(ctx context.Context, req *serverv1.CreateLeaderboardBoardRequest) (*sharedv1.LeaderboardBoard, error) {
	return s.api.CreateLeaderboardBoard(ctx, req)
}

func (s *LeaderboardsService) GetLeaderboardBoard(ctx context.Context, boardID string) (*sharedv1.LeaderboardBoard, error) {
	return s.api.GetLeaderboardBoard(ctx, &serverv1.GetLeaderboardBoardRequest{BoardId: boardID})
}

// ListLeaderboardBoards 整列返回（不分页，每项目 ≤100）。
func (s *LeaderboardsService) ListLeaderboardBoards(ctx context.Context) (*serverv1.ListLeaderboardBoardsResponse, error) {
	return s.api.ListLeaderboardBoards(ctx, &serverv1.ListLeaderboardBoardsRequest{})
}

func (s *LeaderboardsService) ListLeaderboardBoardPeriods(ctx context.Context, boardID string, limit int32) (*serverv1.ListLeaderboardBoardPeriodsResponse, error) {
	return s.api.ListLeaderboardBoardPeriods(ctx, &serverv1.ListLeaderboardBoardPeriodsRequest{BoardId: boardID, Limit: limit})
}

// UpdateLeaderboardBoard 改榜（proto3 optional 语义：未设置 = 不修改）。
func (s *LeaderboardsService) UpdateLeaderboardBoard(ctx context.Context, req *serverv1.UpdateLeaderboardBoardRequest) (*sharedv1.LeaderboardBoard, error) {
	return s.api.UpdateLeaderboardBoard(ctx, req)
}
