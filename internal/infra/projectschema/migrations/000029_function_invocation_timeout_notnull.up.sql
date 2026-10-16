-- B7（functions-v4 转出清理）：function_invocations.timeout_seconds 收敛为
-- NOT NULL——P0.5 两写预占起新行均带行内快照，NULL 仅存于 P0.5 之前的存量行
-- （孤儿恢复对其回退 1h 的兼容分支随之退役）。
-- 回填值 = 平台缺省超时 15s（与 functions.timeout_seconds 列缺省、app 层
-- defaultTimeoutSeconds 同源）。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

UPDATE {{schema}}.function_invocations
SET timeout_seconds = 15
WHERE timeout_seconds IS NULL;

ALTER TABLE {{schema}}.function_invocations
    ALTER COLUMN timeout_seconds SET DEFAULT 15;

ALTER TABLE {{schema}}.function_invocations
    ALTER COLUMN timeout_seconds SET NOT NULL;
