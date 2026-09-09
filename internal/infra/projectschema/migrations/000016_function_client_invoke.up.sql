-- P2 客户端调用面（docs/design/functions-execution-identity-and-triggers.md §4/§5；
-- Rollout P2 行）：functions 策略列 + function_executions 客户端维度两列。
--   functions            client_callable / client_anonymous_allowed（字段保留，
--                        一期禁用——Create/Update 遇 true 显式报错）/ 
--                        client_per_user_limit（每用户限频，>=0）+ 
--                        client_limit_window（minute|hour|day 可配窗口）。
--                        存量全 FALSE ⇒ fail-closed（设计 §4）。
--   function_executions  invoking_user_id（客户端触发时的调用用户）+
--                        client_idempotency_key（幂等键；部分唯一索引
--                        (project, function, user, key) WHERE key <> '' 支撑
--                        两写预占的去重语义——并发同键第二请求 INSERT 冲突）。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.functions
    ADD COLUMN client_callable BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN client_anonymous_allowed BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN client_per_user_limit INTEGER NOT NULL DEFAULT 0
        CONSTRAINT functions_client_per_user_limit_check CHECK (client_per_user_limit >= 0),
    ADD COLUMN client_limit_window TEXT NOT NULL DEFAULT 'day'
        CONSTRAINT functions_client_limit_window_check CHECK (client_limit_window IN ('minute', 'hour', 'day'));

ALTER TABLE {{schema}}.function_executions
    ADD COLUMN invoking_user_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN client_idempotency_key TEXT NOT NULL DEFAULT '';

-- 幂等去重（设计 §4「幂等是 P2 的正确性关键」）：partial 索引只约束带键的
-- 客户端执行行；触发器/server 面行（key = ''）不受影响。唯一索引在预占
-- INSERT 时刻即生效——并发同键第二请求冲突后回读既有行原样返回。
CREATE UNIQUE INDEX function_executions_client_idem_key
    ON {{schema}}.function_executions (project_id, function_id, invoking_user_id, client_idempotency_key)
    WHERE client_idempotency_key <> '';

-- 限频 DB 降级路径的窗口内计数（设计 §5：Redis 故障时按 function_executions
-- 计数——触发来源 client + 调用用户 + 时间窗；依赖 Q7 保留分级先落地，
-- 否则条数式 prune 会裁掉计数行导致少计超发）。
CREATE INDEX function_executions_client_quota
    ON {{schema}}.function_executions (project_id, function_id, invoking_user_id, created_at)
    WHERE trigger_source = 'client';
