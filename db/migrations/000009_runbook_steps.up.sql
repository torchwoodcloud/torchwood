-- Runbook 迁移状态表（docs/design/runbook.md §2.2，D2：状态存服务端控制面）。
-- 只记录"哪些 step 已应用"的事实，不执行资源动作；按项目隔离（project_id
-- 为 TEXT，对齐 000001 的 projects.id）。UNIQUE (project_id, runbook, version)
-- 是 D8 的并发兜底：CAS（expect_prev_version）漏过的双写只剩一个赢家。
-- 引擎默认 runbook 线 = 'default'（多线并行 CLI 不暴露，表留列，§7）。
CREATE TABLE IF NOT EXISTS runbook_steps (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    runbook TEXT NOT NULL,
    version BIGINT NOT NULL,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, runbook, version)
);
