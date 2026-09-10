-- 000018_function_event_triggers 的对称回滚（functions v3 切片 D）。
-- 顺序红线：先 DELETE type='event' 行再缩 CHECK——若存在 event 触发器行，
-- 收缩后的约束会使整表校验失败（ALTER ADD CONSTRAINT 立即 VALIDATE），
-- 回滚将中断；删除事件触发器行是有损操作（事件订阅配置不可恢复），注释
-- 明示后接受——down 本就用于回退到无事件触发器的旧版本。

DELETE FROM {{schema}}.function_triggers WHERE type = 'event';

DROP INDEX IF EXISTS {{schema}}.function_triggers_event_scan;

ALTER TABLE {{schema}}.function_triggers
    DROP CONSTRAINT function_triggers_type_check;

ALTER TABLE {{schema}}.function_triggers
    ADD CONSTRAINT function_triggers_type_check
    CHECK (type IN ('http', 'cron'));
