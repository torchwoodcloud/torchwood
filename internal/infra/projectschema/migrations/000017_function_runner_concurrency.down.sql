-- 000017_function_runner_concurrency 的对称回滚：先删 CHECK 约束再删列。

ALTER TABLE {{schema}}.functions
    DROP CONSTRAINT IF EXISTS functions_concurrency_check;

ALTER TABLE {{schema}}.functions
    DROP COLUMN IF EXISTS concurrency;
