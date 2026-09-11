-- Phase 2：榜配置补 rewards 列（000019 已发布不可改，增量迁移）。
ALTER TABLE {{schema}}.leaderboard_boards
    ADD COLUMN IF NOT EXISTS rewards JSONB NOT NULL DEFAULT '[]';
