import { listQuery, type HttpTransport } from "../http.js";
import type {
  LeaderboardScoreSnapshot,
  LeaderboardSettlement,
  LeaderboardSettlementGrant,
  ListLeaderboardTopResponse,
  SubmitLeaderboardScoreInput,
} from "../types.js";

/** Server API 排行榜面：代任意 subject 提交 + 快照/榜读（leaderboards.write/read）。 */
export class ServerLeaderboardsService {
  constructor(private readonly http: HttpTransport) {}

  async submitLeaderboardScore(
    boardId: string,
    subjectId: string,
    input: SubmitLeaderboardScoreInput,
  ): Promise<LeaderboardScoreSnapshot> {
    return this.http.request<LeaderboardScoreSnapshot>(
      "POST",
      `/v1/server/leaderboards/${encodeURIComponent(boardId)}:submit`,
      { auth: "apiKey", body: { subject_id: subjectId, ...input } },
    );
  }

  async getLeaderboardEntry(
    boardId: string,
    subjectId: string,
    period?: string,
  ): Promise<LeaderboardScoreSnapshot> {
    return this.http.request<LeaderboardScoreSnapshot>(
      "GET",
      `/v1/server/leaderboards/${encodeURIComponent(boardId)}/entries/${encodeURIComponent(subjectId)}`,
      { auth: "apiKey", query: { period } },
    );
  }

  async listLeaderboardTop(
    boardId: string,
    params?: { period?: string; page_size?: number; page_token?: string },
  ): Promise<ListLeaderboardTopResponse> {
    return this.http.request<ListLeaderboardTopResponse>(
      "GET",
      `/v1/server/leaderboards/${encodeURIComponent(boardId)}/top`,
      { auth: "apiKey", query: listQuery(params as never) },
    );
  }

  /** 结算读（Phase 2）：发奖明细 = 用户资产 ledger 的 leaderboard_settlement 引用视图。 */
  async getLeaderboardSettlement(
    boardId: string,
    period: string,
  ): Promise<{
    settlement: LeaderboardSettlement;
    grants: LeaderboardSettlementGrant[];
  }> {
    return this.http.request(
      "GET",
      `/v1/server/leaderboards/${encodeURIComponent(boardId)}/settlements/${encodeURIComponent(period)}`,
      { auth: "apiKey" },
    );
  }

  async listLeaderboardSettlements(
    boardId: string,
    limit?: number,
  ): Promise<{ settlements: LeaderboardSettlement[] }> {
    return this.http.request(
      "GET",
      `/v1/server/leaderboards/${encodeURIComponent(boardId)}/settlements`,
      { auth: "apiKey", query: { limit } },
    );
  }
}
