-- 000024_function_deployment_build_node 的对称回滚：成对 DROP build_node 列。

ALTER TABLE {{schema}}.function_deployments
    DROP COLUMN IF EXISTS build_node;
