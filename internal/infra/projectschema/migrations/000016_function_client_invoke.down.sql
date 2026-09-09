-- 000016_function_client_invoke 的对称回滚。

DROP INDEX IF EXISTS {{schema}}.function_executions_client_quota;
DROP INDEX IF EXISTS {{schema}}.function_executions_client_idem_key;

ALTER TABLE {{schema}}.function_executions
    DROP COLUMN IF EXISTS client_idempotency_key,
    DROP COLUMN IF EXISTS invoking_user_id;

ALTER TABLE {{schema}}.functions
    DROP COLUMN IF EXISTS client_limit_window,
    DROP COLUMN IF EXISTS client_per_user_limit,
    DROP COLUMN IF EXISTS client_anonymous_allowed,
    DROP COLUMN IF EXISTS client_callable;
