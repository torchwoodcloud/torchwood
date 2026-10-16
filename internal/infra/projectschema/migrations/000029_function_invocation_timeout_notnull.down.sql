-- B7 down：退回可空形态。已回填的 15 不可逆推原值（NULL 旧行的原始超时
-- 未落库），仅退约束——旧行为（NULL 回退 1h 孤儿恢复）需配旧版代码。

ALTER TABLE {{schema}}.function_invocations
    ALTER COLUMN timeout_seconds DROP NOT NULL;

ALTER TABLE {{schema}}.function_invocations
    ALTER COLUMN timeout_seconds DROP DEFAULT;
