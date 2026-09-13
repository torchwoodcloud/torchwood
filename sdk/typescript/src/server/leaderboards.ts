import { listQuery, type HttpTransport } from "../http.js";
import type {
  CreateLeaderboardBoardInput,
  LeaderboardBoard,
  LeaderboardScoreSnapshot,
  LeaderboardSettlement,
  LeaderboardSettlementGrant,
  ListLeaderboardTopResponse,
  SubmitLeaderboardScoreInput,
  UpdateLeaderboardBoardInput,
} from "../types.js";

/** Server API 排行榜面：代任意 subject 提交（leaderboards.write）+ 快照/榜读
 * （leaderboards.read）+ board 配置管控（leaderboards.admin，配置面与提交面
 * 刻意分离——提交分值的密钥不得改榜配置）。 */
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

  // —— board 配置管控（预置与门禁自动化；Create/Update 需 leaderboards.admin）——

  /** 幂等建榜：已存在且配置逐字段相等（缺省归一后）→ 200 + 现状（重放安全）；
   * 不等 → 409 ALREADY_EXISTS（错误消息附字段 diff）。 */
  async createLeaderboardBoard(
    id: string,
    input: CreateLeaderboardBoardInput,
  ): Promise<LeaderboardBoard> {
    return this.http.request<LeaderboardBoard>(
      "POST",
      "/v1/server/leaderboards/boards",
      { auth: "apiKey", body: { id, ...input } },
    );
  }

  /** 榜配置读（门禁断言主读点：period/tz/policy/value bounds/client_submit 等全文）。 */
  async getLeaderboardBoard(boardId: string): Promise<LeaderboardBoard> {
    return this.http.request<LeaderboardBoard>(
      "GET",
      `/v1/server/leaderboards/boards/${encodeURIComponent(boardId)}`,
      { auth: "apiKey" },
    );
  }

  async listLeaderboardBoards(): Promise<{ boards: LeaderboardBoard[] }> {
    return this.http.request("GET", "/v1/server/leaderboards/boards", { auth: "apiKey" });
  }

  /** 已有条目的期 key 降序（对账期存在性）。 */
  async listLeaderboardBoardPeriods(
    boardId: string,
    limit?: number,
  ): Promise<{ periods: string[] }> {
    return this.http.request(
      "GET",
      `/v1/server/leaderboards/boards/${encodeURIComponent(boardId)}/periods`,
      { auth: "apiKey", query: { limit } },
    );
  }

  /** 改榜（optional = 不修改；显式清空走 clear_*）。不含 rewards——奖励规则
   * 编辑仅 console owner。 */
  async updateLeaderboardBoard(
    boardId: string,
    input: UpdateLeaderboardBoardInput,
  ): Promise<LeaderboardBoard> {
    return this.http.request<LeaderboardBoard>(
      "PATCH",
      `/v1/server/leaderboards/boards/${encodeURIComponent(boardId)}`,
      { auth: "apiKey", body: input },
    );
  }
}
