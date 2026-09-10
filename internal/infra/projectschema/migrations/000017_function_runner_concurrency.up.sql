-- Functions v3 实例内多路复用（docs/design/functions-v3.md §1.5 透传链/
-- §1.6 兼容迁移；Rollout A 切片二）：functions.concurrency 池策略列。
--   语义：单实例（单 Node 事件循环）内的 per-function 并发上限（§1.1），
--   默认 1 = 与 v2 串行行为等价（fail-closed 惯例），函数作者显式 opt-in
--   1..16（可重入契约：模块级可变全局状态在并发下有竞态，Lambda/Cloud Run
--   同款）。上限 16 为 2026-09-10 owner 拍板（D2：8 实例 × 16 = 128 并发，
--   单机拓扑够用），CHECK 约束兜底。
--   透传链：bun model → domain Function → buildExecution → dispatcher
--   ExecuteRequest.Pool.Concurrency（spawn 时固化进 InstanceRecord，§1.1）。
--   存量 deployment（template_version < 3）执行时降级按 1（fail-safe 不
--   fail-closed，§1.5），重部署获 v3 模板后自然生效。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.functions
    ADD COLUMN concurrency INTEGER NOT NULL DEFAULT 1
        CONSTRAINT functions_concurrency_check CHECK (concurrency BETWEEN 1 AND 16);
