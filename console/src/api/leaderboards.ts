import { api } from "./client";

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

export async function listBoardTop(
  boardId: string,
  params?: { period?: string; page_size?: number; page_token?: string }
): Promise<{ period: string; total: number; entries: LeaderboardTopEntry[]; next_page_token?: string }> {
  const res = await api.get(
    `/console/leaderboards/boards/${encodeURIComponent(boardId)}/top`,
    { params }
  );
  return res.data;
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
