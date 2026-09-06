-- Transactional outbox 事件脊柱终态（基线重定 2026-09-06：000011/000014/
-- 000028 合并）。与租户 schema 文档写同实例、同 COMMIT（event_id 即 outbox PK）。
--   channel —— 显式频道列：经济/文档事件共用同表同管道；文档事件为 NULL。
--   seq —— GENERATED ALWAYS AS IDENTITY（阶段④，redesign §4.5 / §11-J B1）：
--     全局 seq 由 PG 在 INSERT 时分配（OVERRIDING 之外不可手工指定），event_id
--     保持 PK（幂等去重键不变）。顺序承诺（B1 定稿）：单文档全序（行锁保证
--     seq 随提交序）；集合内分配序（跨文档不保证与提交序一致）；seq 有空洞 =
--     回滚事务消耗了 identity 值，不表示丢事件。UNIQUE 索引即位点续传
--    （:changes / last_seq）的物理基础。
CREATE TABLE IF NOT EXISTS document_events_outbox (
    event_id        TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL,
    topic           TEXT NOT NULL,
    payload         JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    available_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts        INT NOT NULL DEFAULT 0,
    dispatched_at   TIMESTAMPTZ,
    published_at    TIMESTAMPTZ,
    channel         TEXT,
    seq             BIGINT GENERATED ALWAYS AS IDENTITY
);

CREATE INDEX IF NOT EXISTS document_events_outbox_poll
    ON document_events_outbox (available_at)
    WHERE published_at IS NULL;

CREATE UNIQUE INDEX document_events_outbox_seq_key
    ON document_events_outbox (seq);

CREATE TABLE IF NOT EXISTS document_events_outbox_dead (
    event_id     TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    topic        TEXT NOT NULL,
    payload      JSONB NOT NULL,
    attempts     INT NOT NULL,
    last_error   TEXT,
    failed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMPTZ NOT NULL,
    channel      TEXT
);

-- NOTIFY 唤醒走 AFTER INSERT 行级触发器：每事件零额外客户端语句（应用侧
-- 逐条 SELECT pg_notify 会使 Bulk 语句数翻倍——R5-P2-6 语句数预算不可回退），
-- 且 PG 对同事务内相同 (channel, payload) 的 NOTIFY 自动合并——execute-tx
-- 100 op 批只投递一次唤醒。NOTIFY 随 commit 投递、回滚即丢弃，与行可见性
-- 天然对齐。
CREATE OR REPLACE FUNCTION tw_outbox_notify() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('tw_outbox', '');
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_events_outbox_notify
    AFTER INSERT ON document_events_outbox
    FOR EACH ROW EXECUTE FUNCTION tw_outbox_notify();
