-- 000014 对称回滚：先删带 CHECK 约束的池策略列，再删投影/快照列。

ALTER TABLE {{schema}}.functions
    DROP CONSTRAINT IF EXISTS functions_max_requests_per_instance_check;
ALTER TABLE {{schema}}.functions
    DROP CONSTRAINT IF EXISTS functions_idle_ttl_seconds_check;
ALTER TABLE {{schema}}.functions
    DROP CONSTRAINT IF EXISTS functions_max_instances_check;
ALTER TABLE {{schema}}.functions
    DROP CONSTRAINT IF EXISTS functions_min_instances_check;

ALTER TABLE {{schema}}.functions
    DROP COLUMN IF EXISTS latest_ready_deployment_id,
    DROP COLUMN IF EXISTS max_requests_per_instance,
    DROP COLUMN IF EXISTS idle_ttl_seconds,
    DROP COLUMN IF EXISTS max_instances,
    DROP COLUMN IF EXISTS min_instances;

ALTER TABLE {{schema}}.function_executions
    DROP COLUMN IF EXISTS timeout_seconds;

ALTER TABLE {{schema}}.function_deployments
    DROP COLUMN IF EXISTS template_version;
