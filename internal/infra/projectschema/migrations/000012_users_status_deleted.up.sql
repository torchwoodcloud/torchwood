-- T-03（安全审计 2026-09-08）：DeleteAccount 匿名化软删终态。
-- users.status CHECK 放宽加入 'deleted'（幂等：先 DROP IF EXISTS 再重建，
-- 与 000008 的约束形态保持一致）。

ALTER TABLE {{schema}}.users DROP CONSTRAINT IF EXISTS users_status_check;

ALTER TABLE {{schema}}.users
    ADD CONSTRAINT users_status_check
    CHECK (status IN ('active', 'inactive', 'blocked', 'deleted'));
