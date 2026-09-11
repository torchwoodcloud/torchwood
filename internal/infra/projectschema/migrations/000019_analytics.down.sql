-- 000019 down：删除 analytics 六表。分区父表 CASCADE 一并删除子分区与
-- BIGSERIAL 序列。占位符 {{schema}} 由 Apply 替换为 quoteIdent。

DROP TABLE IF EXISTS {{schema}}.analytics_events CASCADE;
DROP TABLE IF EXISTS {{schema}}.analytics_event_definitions;
DROP TABLE IF EXISTS {{schema}}.analytics_daily;
DROP TABLE IF EXISTS {{schema}}.analytics_user_days;
DROP TABLE IF EXISTS {{schema}}.analytics_user_first_seen;
DROP TABLE IF EXISTS {{schema}}.analytics_user_deletions;
