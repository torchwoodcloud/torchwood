-- P1 触发器模块（docs/design/functions-execution-identity-and-triggers.md §3；
-- Rollout P1 行）：HTTP + cron 触发器实体与执行记录触发来源列。
--   function_triggers        触发器实体：type ∈ {http, cron}；config JSONB 存
--                            分类型配置（http: response_mode/ack_body/handshake/
--                            body_limit_bytes；cron: expr/misfire）；token 提为
--                            独立列（UNIQUE 索引支撑 /f/{project}/{token} 的
--                            token 查找——设计 §3 拍板「独立列更简单可靠」）；
--                            next_run_at 为 cron 专用（http 恒 NULL）。
--   function_executions      trigger_source（http:{trigger_id} / cron:{trigger_id}
--                            /空=server 面）与 source_ip（HTTP 触发的来源 IP
--                            摘要；该路由不经 gRPC 拦截器链，执行记录即审计
--                            载体——设计 §3「双响应模式」节）。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

CREATE TABLE {{schema}}.function_triggers (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    function_id TEXT NOT NULL REFERENCES {{schema}}.functions(id) ON DELETE CASCADE,
    type TEXT NOT NULL
        CONSTRAINT function_triggers_type_check CHECK (type IN ('http', 'cron')),
    config JSONB NOT NULL,
    -- http 触发器的公开调用 token（128bit 随机，URL 即鉴权）；cron 触发器为
    -- NULL（UNIQUE 索引对 NULL 不去重，多条 cron 触发器合法共存）。
    token TEXT,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    -- cron 专用：下一次计划执行时刻（UTC）；领取即 CAS 推进（先 CAS 后入队）。
    next_run_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 项目/函数维度列表（管理面 ListFunctionTriggers 与函数删除级联后的残留扫描）。
CREATE INDEX function_triggers_project_function
    ON {{schema}}.function_triggers (project_id, function_id);

-- cron 到期扫描：enabled 作前导键、next_run_at 升序；partial 收窄 type='cron'
-- （type 已由 partial 条件保证，不重复进键）。
CREATE INDEX function_triggers_cron_due
    ON {{schema}}.function_triggers (enabled, next_run_at)
    WHERE type = 'cron';

-- token 查找（/f/{project_id}/{token} 热路径）；UNIQUE 保证 token 不可猜且
-- 全局唯一，NULL（cron）不去重。
CREATE UNIQUE INDEX function_triggers_token_key
    ON {{schema}}.function_triggers (token);

ALTER TABLE {{schema}}.function_executions
    ADD COLUMN trigger_source TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_ip TEXT NOT NULL DEFAULT '';
