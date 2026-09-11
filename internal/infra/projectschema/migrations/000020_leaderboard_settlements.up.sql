-- 排行榜 Phase 2：结榜发奖。占位符 {{schema}} 由 Apply 替换。
-- 结算行是"期 → 奖励发放"的账本（不随条目 retention 清理）；
-- 发放明细行是重跑账本（按幂等键续跑，已发行不重复发）。

CREATE TABLE IF NOT EXISTS {{schema}}.leaderboard_settlements (
    id             TEXT PRIMARY KEY,
    project_id     TEXT NOT NULL REFERENCES public.projects(id),
    board_id       TEXT NOT NULL,
    period_key     TEXT NOT NULL,
    -- pending | settling | settled | error | voided
    status         TEXT NOT NULL DEFAULT 'settling',
    sealed_at      TIMESTAMPTZ NOT NULL,
    settled_at     TIMESTAMPTZ,
    rules_snapshot JSONB NOT NULL,
    entry_count    BIGINT NOT NULL DEFAULT 0,
    grant_count    INTEGER NOT NULL DEFAULT 0,
    error          TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT leaderboard_settlements_status CHECK (status IN ('pending', 'settling', 'settled', 'error', 'voided')),
    CONSTRAINT leaderboard_settlements_board_period UNIQUE (board_id, period_key)
);

CREATE INDEX IF NOT EXISTS leaderboard_settlements_board
    ON {{schema}}.leaderboard_settlements (project_id, board_id, period_key DESC);

CREATE TABLE IF NOT EXISTS {{schema}}.leaderboard_settlement_grants (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES public.projects(id),
    settlement_id   TEXT NOT NULL REFERENCES {{schema}}.leaderboard_settlements(id) ON DELETE CASCADE,
    rule_index      INTEGER NOT NULL,
    subject_id      TEXT NOT NULL,
    asset_code      TEXT NOT NULL,
    amount          BIGINT NOT NULL,
    idempotency_key TEXT NOT NULL,
    -- pending | granted | failed | voided
    status          TEXT NOT NULL DEFAULT 'pending',
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT leaderboard_settlement_grants_status CHECK (status IN ('pending', 'granted', 'failed', 'voided')),
    CONSTRAINT leaderboard_settlement_grants_unique UNIQUE (settlement_id, rule_index, subject_id),
    CONSTRAINT leaderboard_settlement_grants_amount CHECK (amount > 0)
);

CREATE INDEX IF NOT EXISTS leaderboard_settlement_grants_settlement
    ON {{schema}}.leaderboard_settlement_grants (settlement_id, status);
