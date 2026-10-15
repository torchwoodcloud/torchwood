-- 000028_functions_v4_target 的对称回滚（Functions v4 破坏波）。
-- 有损声明：function_deployments / function_variables 行、池策略值、
-- runtime/entrypoint/spec 值与 target 配置均不可恢复（列/表已 DROP）——
-- down 用于回退到 v3 形态继续跑旧版本二进制，部署面数据由重新部署重建
--（down 本就用于整卷回滚，owner 已接受）。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

-- ① functions：target 列族退役，v3 列族按原迁移（000003/000014/000017）
-- 的定义复位。
ALTER TABLE {{schema}}.functions
    DROP COLUMN target_url,
    DROP COLUMN target_secret_enc,
    DROP COLUMN target_previous_secret_enc,
    DROP COLUMN target_secret_rotated_at,
    DROP COLUMN target_secret_fingerprint;

ALTER TABLE {{schema}}.functions
    ADD COLUMN runtime TEXT NOT NULL DEFAULT '',
    ADD COLUMN entrypoint TEXT NOT NULL DEFAULT 'index.main',
    ADD COLUMN spec TEXT NOT NULL DEFAULT 'shared-1x',
    ADD COLUMN min_instances INTEGER NOT NULL DEFAULT 0
        CONSTRAINT functions_min_instances_check CHECK (min_instances >= 0),
    ADD COLUMN max_instances INTEGER NOT NULL DEFAULT 2
        CONSTRAINT functions_max_instances_check CHECK (max_instances >= 1),
    ADD COLUMN idle_ttl_seconds INTEGER NOT NULL DEFAULT 300
        CONSTRAINT functions_idle_ttl_seconds_check CHECK (idle_ttl_seconds >= 30),
    ADD COLUMN max_requests_per_instance INTEGER NOT NULL DEFAULT 1000
        CONSTRAINT functions_max_requests_per_instance_check CHECK (max_requests_per_instance >= 1),
    ADD COLUMN concurrency INTEGER NOT NULL DEFAULT 1
        CONSTRAINT functions_concurrency_check CHECK (concurrency BETWEEN 1 AND 16),
    ADD COLUMN latest_ready_deployment_id TEXT;

-- ② 退役表按原迁移（000003 + 000013/14/23/24/25）定义重建（空表）。
CREATE TABLE {{schema}}.function_deployments (
    id          TEXT PRIMARY KEY,
    function_id TEXT NOT NULL REFERENCES {{schema}}.functions(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL,
    size        BIGINT NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'pending',
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    template_version INTEGER,
    source_type TEXT NOT NULL DEFAULT 'zip'
        CONSTRAINT function_deployments_source_type_check CHECK (source_type IN ('zip', 'git', 'image')),
    source_url TEXT NOT NULL DEFAULT '',
    source_ref TEXT NOT NULL DEFAULT '',
    source_dir TEXT NOT NULL DEFAULT '',
    context_sha256 TEXT NOT NULL DEFAULT '',
    build_node TEXT NOT NULL DEFAULT '',
    runtime TEXT NOT NULL DEFAULT ''
);
CREATE INDEX function_deployments_function_idx ON {{schema}}.function_deployments (function_id);

CREATE TABLE {{schema}}.function_variables (
    id          TEXT PRIMARY KEY,
    function_id TEXT NOT NULL REFERENCES {{schema}}.functions(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL,
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,
    kind        TEXT NOT NULL DEFAULT 'text'
        CONSTRAINT function_variables_kind_check CHECK (kind IN ('text', 'secret')),
    UNIQUE (function_id, key)
);

-- ③ function_invocations 改回 function_executions（索引随迁），并复位
-- deployment_id 列 + 外键。
ALTER INDEX {{schema}}.function_invocations_client_quota
    RENAME TO function_executions_client_quota;
ALTER INDEX {{schema}}.function_invocations_client_idem_key
    RENAME TO function_executions_client_idem_key;
ALTER INDEX {{schema}}.function_invocations_function_idx
    RENAME TO function_executions_function_idx;

ALTER TABLE {{schema}}.function_invocations
    RENAME TO function_executions;

ALTER TABLE {{schema}}.function_executions
    ADD COLUMN deployment_id TEXT
    REFERENCES {{schema}}.function_deployments(id) ON DELETE CASCADE;
