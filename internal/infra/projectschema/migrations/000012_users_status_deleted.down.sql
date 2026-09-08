-- 回滚：仅当无 'deleted' 行时才能收回约束放宽；有软删行时保持约束不动
--（幂等 no-op，避免回滚路径炸在存量数据上）。

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM {{schema}}.users WHERE status = 'deleted'
    ) THEN
        ALTER TABLE {{schema}}.users DROP CONSTRAINT IF EXISTS users_status_check;
        ALTER TABLE {{schema}}.users
            ADD CONSTRAINT users_status_check
            CHECK (status IN ('active', 'inactive', 'blocked'));
    END IF;
END $$;
