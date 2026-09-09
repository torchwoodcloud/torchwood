-- 000015_function_triggers 的对称回滚。

DROP TABLE IF EXISTS {{schema}}.function_triggers;

ALTER TABLE {{schema}}.function_executions
    DROP COLUMN IF EXISTS trigger_source,
    DROP COLUMN IF EXISTS source_ip;
