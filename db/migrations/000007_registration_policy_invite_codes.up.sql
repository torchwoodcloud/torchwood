-- T-03（安全审计 2026-09-08）：项目级注册策略 + 邀请码。
-- 默认 open 保持存量行为不变；邀请码存控制面（与 api_keys 同为平台管理的
-- 项目级凭证，跨项目键 project_id 隔离），消费经单语句 UPDATE 原子化。

ALTER TABLE projects
    ADD COLUMN registration_policy TEXT NOT NULL DEFAULT 'open';

ALTER TABLE projects
    ADD CONSTRAINT projects_registration_policy_check
    CHECK (registration_policy IN ('open', 'invite_only', 'closed'));

CREATE TABLE invite_codes (
    id          TEXT PRIMARY KEY,
    project_id  TEXT        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    code        TEXT        NOT NULL,
    max_uses    INTEGER     NOT NULL DEFAULT 1 CHECK (max_uses >= 1),
    used_count  INTEGER     NOT NULL DEFAULT 0,
    expire_at   TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- （project_id, code) 唯一：码在项目内唯一，跨项目同名合法。
    CONSTRAINT invite_codes_project_code_key UNIQUE (project_id, code)
);

CREATE INDEX idx_invite_codes_project ON invite_codes(project_id, created_at DESC);
