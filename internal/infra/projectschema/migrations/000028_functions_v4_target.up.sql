-- Functions v4 破坏波（docs/design/functions-v4.md §2 重命名 / §3 target /
-- §7 记账 / §10 退役清单；一步到位，无并存期）：
--   function_executions → function_invocations 改名（幂等唯一索引/限频
--     partial 索引/函数索引随迁）；deployment_id 列退役（部署面消解）；
--   function_deployments / function_variables 表退役（D9——部署台账真身在
--     fleetly/用户侧，variables 的 env 注入点本就是部署侧）；
--   functions 池策略五列 + runtime/entrypoint/spec/latest_ready_deployment_id
--     退役（池策略与运行时归 substrate）；ADD target 列族（url/secretbox
--     密文双列 + 指纹 + rotated_at——v4 §3 单一 endpoint target，模式照抄
--     000027 webhook_subscriptions）。
-- 状态机收敛（v4 §7）：building 态随补构建退役（SQL 无 CHECK 约束承载状态
-- 词表，收敛发生在 app 层词表）；queued/running/completed/failed 四态。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

-- ① function_invocations：先脱部署外键（deployment_id 列带 FK →
-- function_deployments），再删列、改名、迁索引。
ALTER TABLE {{schema}}.function_executions
    DROP COLUMN deployment_id;

ALTER TABLE {{schema}}.function_executions
    RENAME TO function_invocations;

ALTER INDEX {{schema}}.function_executions_function_idx
    RENAME TO function_invocations_function_idx;
ALTER INDEX {{schema}}.function_executions_client_idem_key
    RENAME TO function_invocations_client_idem_key;
ALTER INDEX {{schema}}.function_executions_client_quota
    RENAME TO function_invocations_client_quota;

-- ② 退役表（FK 由 function_invocations 侧已解除；function_variables 的
-- FK 随表删除）。
DROP TABLE {{schema}}.function_deployments;
DROP TABLE {{schema}}.function_variables;

-- ③ functions：池五列 + runtime 系列 + latest 指针退役。
ALTER TABLE {{schema}}.functions
    DROP COLUMN runtime,
    DROP COLUMN entrypoint,
    DROP COLUMN spec,
    DROP COLUMN min_instances,
    DROP COLUMN max_instances,
    DROP COLUMN idle_ttl_seconds,
    DROP COLUMN max_requests_per_instance,
    DROP COLUMN concurrency,
    DROP COLUMN latest_ready_deployment_id;

-- ④ target 列族（v4 §3；密钥本体 secretbox 加密落库 enc:v1: 前缀，API 面
-- 只出指纹，明文仅创建/轮换一次性回显——实施勘误 2026-10-15）。存量函数
-- 回填空串/NULL = 无 target（调用面按「target 未配置」fail-closed 拒绝）。
ALTER TABLE {{schema}}.functions
    ADD COLUMN target_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN target_secret_enc TEXT NOT NULL DEFAULT '',
    ADD COLUMN target_previous_secret_enc TEXT,
    ADD COLUMN target_secret_rotated_at TIMESTAMPTZ,
    ADD COLUMN target_secret_fingerprint TEXT NOT NULL DEFAULT '';
