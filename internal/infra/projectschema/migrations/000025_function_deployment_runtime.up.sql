-- Functions 运行时指定（docs/design/functions-runtime-selection.md §4，
-- 切片 S3）：function_deployments 增 runtime 快照列。
--   runtime  构建所用 runtime ID（node-24.0 / go-1.26 / image …）。
-- deployment = 源 + 运行时 + 模板版本的完整不可变快照：INSERT 期写全、
-- 之后不可变——不登记进 UpdateDeployment 列白名单（bun 更新写规范：
-- 不可变列漏登记正是期望行为）；补构建/审计以行内快照为准，不随后续
-- UpdateFunction 的 runtime 变更漂移（快照忠实性，§3 弱点由此消除）。
-- 存量行回填 = 各自函数的当前 fn.runtime（回填前 fn.runtime 不可变，
-- 回填后二者语义解耦）。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.function_deployments
    ADD COLUMN runtime TEXT NOT NULL DEFAULT '';

UPDATE {{schema}}.function_deployments AS fd
   SET runtime = f.runtime
  FROM {{schema}}.functions AS f
 WHERE fd.function_id = f.id
   AND fd.runtime = '';
