-- outbox 补偿/重放查询面索引（:changes?since_seq= 双面 RPC 与 WS subscribe
-- last_seq 重放，internal/infra/documentdb/postgres_changes.go scanChanges）：
--   WHERE project_id = ? AND topic = ? AND seq > ? ORDER BY seq LIMIT ?
-- 既有索引只有 (available_at) 部分索引（投递轮询专用）与 (seq) 唯一索引，
-- 均不含 project/topic 维度——该查询只能走全局 seq 范围扫后逐行过滤其余
-- 项目的行，代价随全实例 24h 保留窗口内的写入量线性增长。本复合索引把
-- 扫描面收窄到（项目, topic）内。位点续传语义（seq 全序、单调）仍由 seq
-- 唯一索引承担，本索引只服务扫描收窄，不改变事件序承诺。
-- 不用 CREATE INDEX CONCURRENTLY：golang-migrate 每个迁移在单事务内执行，
-- CONCURRENTLY 无法在事务块中运行；当前部署规模下建索引期间的短暂
-- SHARE 锁（阻塞 outbox INSERT/UPDATE 写、不阻塞读）可接受。
CREATE INDEX IF NOT EXISTS document_events_outbox_project_topic_seq
    ON document_events_outbox (project_id, topic, seq);
