-- 回退即退场（设计 §5：服务端 RPC 不被调用则闲置无副作用；
-- ON DELETE CASCADE 随项目清理，DROP TABLE 只清独立状态）。
DROP TABLE IF EXISTS runbook_steps;
