-- 审计日志查询面索引（roadmap §3.4「管理面写操作可查」）：
--   - project + action 组合：Console/CLI 按"操作了什么"过滤的最常用路径；
--   - 裸 created_at：平台 admin 的 all_projects 全量视图无项目谓词，
--     借此避免大表全量排序（project_id 维度已有 000001 的
--     idx_audit_logs_project_created 覆盖）。
CREATE INDEX IF NOT EXISTS idx_audit_logs_project_action_created
    ON audit_logs (project_id, action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created
    ON audit_logs (created_at DESC);
