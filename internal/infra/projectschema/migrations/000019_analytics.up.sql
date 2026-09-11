-- Analytics 事件分析六表（docs/design/analytics.md §5，PR1 地基）。
-- 事件通道独立于文档层（D1）：项目 schema 静态系统表、不进 outbox、
-- 无 RLS、不建 project_id 列（schema 即项目边界，项目删除 =
-- DROP SCHEMA CASCADE 零额外清理）。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

-- ① 原始事件：月度 RANGE 分区，追加只写（D5）。PK 含分区键；id 为
--    BIGSERIAL 调试序（PG<17 分区表限 IDENTITY，故用 serial 形态；
--    无唯一约束——接受重复 D14，at-least-once）。
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_events (
    id           BIGSERIAL NOT NULL,
    name         TEXT NOT NULL,
    user_id      TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL DEFAULT 'client' CHECK (source IN ('client', 'server')),
    platform     TEXT NOT NULL DEFAULT '',      -- 通用上下文维度（web/ios/android/mp...）
    app_version  TEXT NOT NULL DEFAULT '',
    occurred_at  TIMESTAMPTZ NOT NULL,           -- 事件时间（客户端，钳制窗兜底 D4）
    ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    props        JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- 父表索引级联子分区（PG11+ partitioned index）。name+time：趋势/拆解主路；
-- user+time：下钻与合规点删（D10）；BRIN：时间范围扫描兜底；GIN jsonb_path_ops：
-- props 维度查询。
CREATE INDEX IF NOT EXISTS analytics_events_name_time
    ON {{schema}}.analytics_events (name, occurred_at DESC);
CREATE INDEX IF NOT EXISTS analytics_events_user_time
    ON {{schema}}.analytics_events (user_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS analytics_events_brin
    ON {{schema}}.analytics_events USING brin (occurred_at);
CREATE INDEX IF NOT EXISTS analytics_events_props_gin
    ON {{schema}}.analytics_events USING gin (props jsonb_path_ops);

-- 迁移静态预建当月+次月分区（字面量，2026-09 落地时点）+ DEFAULT 兜底
-- （钳制窗 [now-24h, now+5min] 保证跨月竞态仅落 DEFAULT；后续预建/
-- 裁剪随 maintenance worker，见设计 §7/PR5——P0 若独立运行超 ~1 个月
-- 未合入 worker，须提前分区预建循环，见执行计划 PR2 附注）。
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_events_2026_09
    PARTITION OF {{schema}}.analytics_events
    FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_events_2026_10
    PARTITION OF {{schema}}.analytics_events
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_events_default
    PARTITION OF {{schema}}.analytics_events DEFAULT;

-- ② 事件字典（D12：摄取时 upsert；软上限 1000 的锚点；Console 发现入口）。
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_event_definitions (
    name        TEXT PRIMARY KEY,
    first_seen  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    total_30d   BIGINT NOT NULL DEFAULT 0      -- rollup 维护（排序用）
);

-- ③ 事件×日聚合（D6：精确 UV；用户删除后为近似口径，文档声明）。
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_daily (
    day          DATE NOT NULL,
    name         TEXT NOT NULL,
    total        BIGINT NOT NULL,
    unique_users BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (day, name)
);

-- ④ 用户×日活跃集（UV/留存共同基座；行数 = Σ日活，随 raw 保留期同步修剪）。
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_user_days (
    user_id TEXT NOT NULL,
    day     DATE NOT NULL,
    events  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, day)
);

-- ⑤ 用户首见/末见（cohort 锚点 + 下钻档案）。
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_user_first_seen (
    user_id    TEXT PRIMARY KEY,
    first_day  DATE NOT NULL,
    last_day   DATE NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ⑥ 合规清洗队列（注销钩子写入，worker 消费，D10；PR5 接线）。
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_user_deletions (
    user_id     TEXT PRIMARY KEY,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    done_at     TIMESTAMPTZ
);
