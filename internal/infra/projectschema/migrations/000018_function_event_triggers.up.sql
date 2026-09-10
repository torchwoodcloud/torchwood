-- Functions v3 切片 D：数据库事件触发器（docs/design/functions-v3.md §4.1/D12，
-- Rollout D 行）。function_triggers.type 的 CHECK 约束扩展为
-- ('http', 'cron', 'event')——事件触发器复用既有四方法管理 RPC 与同一张表，
-- 订阅串存 config JSONB 的 events 数组（TriggerConfig.Events）。
-- 表小（每函数触发器个位数），直接 VALIDATE 不用 NOT VALID，与 000015
-- 风格一致。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.function_triggers
    DROP CONSTRAINT function_triggers_type_check;

ALTER TABLE {{schema}}.function_triggers
    ADD CONSTRAINT function_triggers_type_check
    CHECK (type IN ('http', 'cron', 'event'));

-- 事件触发器订阅匹配器的周期快照扫描（worker eventTriggerConsumer，15s）：
-- 按项目加载启用的 event 触发器。partial 索引与 cron 的
-- function_triggers_cron_due 同款风格（type 由 partial 条件保证，不重复进键）。
CREATE INDEX function_triggers_event_scan
    ON {{schema}}.function_triggers (project_id)
    WHERE type = 'event' AND enabled;
