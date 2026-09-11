-- 排行榜：榜配置 + 条目。占位符 {{schema}} 由 Apply 替换。
-- 条目主键 (board_id, period_key, subject_id) 即模型内建去重：一主体一期一条。

CREATE TABLE IF NOT EXISTS {{schema}}.leaderboard_boards (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL REFERENCES public.projects(id),
    sort              TEXT NOT NULL DEFAULT 'desc',
    tiebreak_order    TEXT,
    tie_break         TEXT NOT NULL DEFAULT 'parallel',
    period_kind       TEXT NOT NULL DEFAULT 'none',
    period_tz         TEXT NOT NULL DEFAULT 'UTC',
    policy            TEXT NOT NULL DEFAULT 'best',
    value_min         BIGINT,
    value_max         BIGINT,
    client_submit     BOOLEAN NOT NULL DEFAULT FALSE,
    per_subject_limit INTEGER NOT NULL DEFAULT 100,
    retention_periods INTEGER NOT NULL DEFAULT 0,
    subject_kind      TEXT NOT NULL DEFAULT 'user',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT leaderboard_boards_sort CHECK (sort IN ('asc', 'desc')),
    CONSTRAINT leaderboard_boards_tiebreak_order CHECK (tiebreak_order IS NULL OR tiebreak_order IN ('asc', 'desc')),
    CONSTRAINT leaderboard_boards_tie_break CHECK (tie_break IN ('parallel', 'earliest', 'latest')),
    CONSTRAINT leaderboard_boards_period_kind CHECK (period_kind IN ('none', 'daily', 'weekly', 'monthly')),
    CONSTRAINT leaderboard_boards_policy CHECK (policy IN ('best', 'latest', 'sum')),
    CONSTRAINT leaderboard_boards_value_range CHECK (value_min IS NULL OR value_max IS NULL OR value_min <= value_max),
    CONSTRAINT leaderboard_boards_per_subject_limit CHECK (per_subject_limit >= 1 AND per_subject_limit <= 10000),
    CONSTRAINT leaderboard_boards_retention CHECK (retention_periods >= 0)
);

CREATE INDEX IF NOT EXISTS leaderboard_boards_project
    ON {{schema}}.leaderboard_boards (project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS {{schema}}.leaderboard_entries (
    board_id       TEXT NOT NULL REFERENCES {{schema}}.leaderboard_boards(id) ON DELETE CASCADE,
    project_id     TEXT NOT NULL REFERENCES public.projects(id),
    period_key     TEXT NOT NULL,
    subject_id     TEXT NOT NULL,
    value          BIGINT NOT NULL,
    tiebreak_value BIGINT,
    submit_count   INTEGER NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT leaderboard_entries_pk PRIMARY KEY (board_id, period_key, subject_id),
    CONSTRAINT leaderboard_entries_submit_count CHECK (submit_count >= 1)
);

-- rank 计数（count FILTER 值域前缀扫描）与全序分页共用一条复合索引。
-- 方向组合取最常见形态（desc 榜 + tiebreak asc + earliest）；其余方向组合
-- 由 planner 退化增量排序，v1 量级（单期 ≤10 万行）可接受，索引策略对 API 不可见。
CREATE INDEX IF NOT EXISTS leaderboard_entries_rank
    ON {{schema}}.leaderboard_entries
       (project_id, board_id, period_key, value DESC, tiebreak_value ASC, updated_at ASC, subject_id ASC);
