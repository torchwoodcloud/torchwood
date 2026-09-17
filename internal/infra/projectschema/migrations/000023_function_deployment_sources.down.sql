-- 000023_function_deployment_sources 的对称回滚：成对 DROP 五源列。

ALTER TABLE {{schema}}.function_deployments
    DROP COLUMN IF EXISTS source_type,
    DROP COLUMN IF EXISTS source_url,
    DROP COLUMN IF EXISTS source_ref,
    DROP COLUMN IF EXISTS source_dir,
    DROP COLUMN IF EXISTS context_sha256;
