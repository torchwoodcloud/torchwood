import { listQuery, type HttpTransport } from "../http.js";
import type {
  LeaderboardScoreSnapshot,
  ListLeaderboardTopResponse,
  SubmitLeaderboardScoreInput,
} from "../types.js";

/**
 * 终端用户排行榜面。submit 的 subject 恒从 session 派生——client 面
 * 不存在"代他人提交"这回事；board 未开 client_submit 时 PERMISSION_DENIED。
 */
export class ClientLeaderboardsService {
  constructor(private readonly http: HttpTransport) {}

  async submitLeaderboardScore(
    boardId: string,
    input: SubmitLeaderboardScoreInput,
  ): Promise<LeaderboardScoreSnapshot> {
    return this.http.request<LeaderboardScoreSnapshot>(
      "POST",
      `/v1/leaderboards/${encodeURIComponent(boardId)}:submit`,
      { body: input },
    );
  }

  async getMyLeaderboardEntry(boardId: string, period?: string): Promise<LeaderboardScoreSnapshot> {
    return this.http.request<LeaderboardScoreSnapshot>(
      "GET",
      `/v1/leaderboards/${encodeURIComponent(boardId)}/me`,
      { query: { period } },
    );
  }

  async listLeaderboardTop(
    boardId: string,
    params?: { period?: string; page_size?: number; page_token?: string },
  ): Promise<ListLeaderboardTopResponse> {
    return this.http.request<ListLeaderboardTopResponse>(
      "GET",
      `/v1/leaderboards/${encodeURIComponent(boardId)}/top`,
      { query: listQuery(params as never) },
    );
  }
}
