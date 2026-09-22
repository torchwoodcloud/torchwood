import { api } from "./client";
import { pageQuery, type ListParams, type Page } from "./pagination";

export interface LeaderboardBoard {
  id: string;
  sort?: string;
  tiebreak_order?: string;
  tie_break?: string;
  period_kind?: string;
  period_tz?: string;
  policy?: string;
  value_min?: string;
  value_max?: string;
  client_submit?: boolean;
  per_subject_submit_limit?: number;
  retention_periods?: number;
  subject_kind?: string;
  created_at?: string;
  updated_at?: string;
}

export interface LeaderboardTopEntry {
  subject_id: string;
  value: string;
  tiebreak_value?: string;
  rank: number;
  position: number;
  updated_at?: string;
}

export interface LeaderboardScoreSnapshot {
  board_id: string;
  period: string;
  total: number;
  entry?: {
    subject_id: string;
    value: string;
    tiebreak_value?: string;
    submit_count?: number;
    updated_at?: string;
  };
  rank?: number;
  position?: number;
  below?: number;
}

export const LEADERBOARD_SORTS = ["desc", "asc"] as const;
export const LEADERBOARD_TIE_BREAKS = ["parallel", "earliest", "latest"] as const;
export const LEADERBOARD_PERIOD_KINDS = ["daily", "weekly", "monthly", "none"] as const;
export const LEADERBOARD_POLICIES = ["best", "latest", "sum"] as const;

export interface LeaderboardRewardRule {
  rank_min?: number;
  rank_max?: number;
  value_min?: string;
  asset_code: string;
  amount: string;
}

export interface CreateBoardInput {
  id: string;
  sort?: string;
  tiebreak_order?: string;
  tie_break?: string;
  period_kind?: string;
  period_tz?: string;
  policy?: string;
  value_min?: string;
  value_max?: string;
  client_submit?: boolean;
  per_subject_submit_limit?: number;
  retention_periods?: number;
  subject_kind?: string;
  rewards?: LeaderboardRewardRule[];
}

export async function listBoards(): Promise<LeaderboardBoard[]> {
  const res = await api.get<{ boards: LeaderboardBoard[] }>(
    "/console/leaderboards/boards"
  );
  return res.data.boards ?? [];
}

export async function getBoard(boardId: string): Promise<LeaderboardBoard> {
  const res = await api.get<LeaderboardBoard>(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}`
  );
  return res.data;
}

export async function createBoard(input: CreateBoardInput): Promise<LeaderboardBoard> {
  const res = await api.post<LeaderboardBoard>("/console/leaderboards/boards", input);
  return res.data;
}

export async function updateBoard(
  boardId: string,
  input: Partial<CreateBoardInput> & {
    clear_tiebreak?: boolean;
    clear_value_bounds?: boolean;
    sort?: string;
    tie_break?: string;
    period_kind?: string;
    policy?: string;
  }
): Promise<LeaderboardBoard> {
  const res = await api.patch<LeaderboardBoard>(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}`,
    input
  );
  return res.data;
}

export async function deleteBoard(boardId: string): Promise<void> {
  await api.delete(`/console/leaderboards/boards/${encodeURIComponent(boardId)}`);
}

export async function listBoardPeriods(boardId: string): Promise<string[]> {
  const res = await api.get<{ periods: string[] }>(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}/periods`
  );
  return res.data.periods ?? [];
}

// Top 列表对接服务端分页（契约说明见 pagination.ts）：服务端按 board 声明
// 排序返回窗口排名（rank/position 按 total 全量计算，翻页不改变名次），
// offset 型 page_token；响应额外携带 period 与 total（此处 total 是该期真实
// 总条数，可直接展示）。
export async function listBoardTop(
  boardId: string,
  params: ListParams & { period?: string }
): Promise<Page<LeaderboardTopEntry> & { period: string; total: number }> {
  const res = await api.get<{
    period: string;
    total: number;
    entries: LeaderboardTopEntry[];
    next_page_token?: string;
  }>(`/console/leaderboards/boards/${encodeURIComponent(boardId)}/top`, {
    params: { ...pageQuery(params), period: params.period || undefined },
  });
  return {
    rows: res.data.entries ?? [],
    nextPageToken: res.data.next_page_token,
    period: res.data.period,
    total: res.data.total,
  };
}

export async function getBoardEntry(
  boardId: string,
  subjectId: string,
  period?: string
): Promise<LeaderboardScoreSnapshot> {
  const res = await api.get<LeaderboardScoreSnapshot>(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}/entries/${encodeURIComponent(subjectId)}`,
    { params: { period } }
  );
  return res.data;
}

export async function deleteBoardEntry(
  boardId: string,
  period: string,
  subjectId: string
): Promise<void> {
  await api.delete(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}/entries/${encodeURIComponent(subjectId)}`,
    { params: { period } }
  );
}

export interface LeaderboardSettlement {
  board_id: string;
  period: string;
  status: string;
  sealed_at?: string;
  settled_at?: string;
  entry_count?: number;
  grant_count?: number;
  error?: string;
  rules?: LeaderboardRewardRule[];
  created_at?: string;
  updated_at?: string;
}

export interface LeaderboardSettlementGrant {
  rule_index: number;
  subject_id: string;
  asset_code: string;
  amount: string;
  idempotency_key: string;
  status: string;
  error?: string;
}

export async function listSettlements(
  boardId: string,
  limit?: number
): Promise<LeaderboardSettlement[]> {
  const res = await api.get<{ settlements: LeaderboardSettlement[] }>(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}/settlements`,
    { params: { limit } }
  );
  return res.data.settlements ?? [];
}

export async function getSettlement(
  boardId: string,
  period: string
): Promise<{ settlement: LeaderboardSettlement; grants: LeaderboardSettlementGrant[] }> {
  const res = await api.get(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}/settlements/${encodeURIComponent(period)}`
  );
  return res.data;
}

export async function voidSettlement(boardId: string, period: string): Promise<void> {
  await api.post(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}/settlements/${encodeURIComponent(period)}:void`
  );
}

export async function rerunSettlement(boardId: string, period: string): Promise<void> {
  await api.post(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}/settlements/${encodeURIComponent(period)}:rerun`
  );
}
