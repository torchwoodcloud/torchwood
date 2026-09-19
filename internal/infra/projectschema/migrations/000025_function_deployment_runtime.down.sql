-- 000025_function_deployment_runtime 的对称回滚：DROP runtime 快照列。

ALTER TABLE {{schema}}.function_deployments
    DROP COLUMN IF EXISTS runtime;
