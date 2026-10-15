-- 000027_webhooks 的对称回滚（Webhooks 一等公民子系统）。
-- 顺序红线：先删 webhook_deliveries（外键引用方）再删
-- webhook_subscriptions。投递行与订阅配置均不可恢复（DLQ/订阅是有损删除），
-- 注释明示后接受——down 本就用于回退到无 webhooks 子系统的旧版本。

DROP INDEX IF EXISTS {{schema}}.webhook_deliveries_terminal_created;
DROP INDEX IF EXISTS {{schema}}.webhook_deliveries_idem;
DROP INDEX IF EXISTS {{schema}}.webhook_deliveries_webhook_created;
DROP INDEX IF EXISTS {{schema}}.webhook_deliveries_due;
DROP TABLE IF EXISTS {{schema}}.webhook_deliveries;

DROP INDEX IF EXISTS {{schema}}.webhook_subscriptions_enabled_scan;
DROP INDEX IF EXISTS {{schema}}.webhook_subscriptions_project;
DROP TABLE IF EXISTS {{schema}}.webhook_subscriptions;
