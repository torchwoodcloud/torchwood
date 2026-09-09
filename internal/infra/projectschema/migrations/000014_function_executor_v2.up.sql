-- P0.5 执行器 v2（docs/design/functions-execution-identity-and-triggers.md §6；
-- Rollout P0.5 行）：常驻 runner 为默认执行模型。本迁移只落数据面列：
--   functions.*            池策略（平台默认 + per-function 覆盖，语义见 §6 池策略）
--   functions.latest_ready_deployment_id   热路径清账（§6 约束③）：selectDeployment
--                          优先读该列，消灭 ListDeployments 全量拉取；可空，
--                          NULL 回退既有全量列表逻辑
--   function_executions.timeout_seconds    快路径孤儿判定的 staleAfter 依据
--                          （两写预占记账 §6 约束①：staleAfter = 行超时 + 120s 宽限）
--   function_deployments.template_version  模板版本化重建（runner 属镜像模板层
--                          变更，存量 deployment 按新模板重建）
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.functions
    ADD COLUMN min_instances INTEGER NOT NULL DEFAULT 0
        CONSTRAINT functions_min_instances_check CHECK (min_instances >= 0),
    ADD COLUMN max_instances INTEGER NOT NULL DEFAULT 2
        CONSTRAINT functions_max_instances_check CHECK (max_instances >= 1),
    ADD COLUMN idle_ttl_seconds INTEGER NOT NULL DEFAULT 300
        CONSTRAINT functions_idle_ttl_seconds_check CHECK (idle_ttl_seconds >= 30),
    ADD COLUMN max_requests_per_instance INTEGER NOT NULL DEFAULT 1000
        CONSTRAINT functions_max_requests_per_instance_check CHECK (max_requests_per_instance >= 1),
    ADD COLUMN latest_ready_deployment_id TEXT;

ALTER TABLE {{schema}}.function_executions
    ADD COLUMN timeout_seconds INTEGER;

ALTER TABLE {{schema}}.function_deployments
    ADD COLUMN template_version INTEGER;
