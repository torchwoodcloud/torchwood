-- 退役每项目 legacy catalog 四表（阶段②包 A）：catalog 元数据已全局化至
-- public 两表（db/migrations/000003）。存量项目 schema 由 EnsureAll / Scoped
-- 重放本版本清理；POC 无数据搬迁（新库 000001 已不再创建，本版本 IF EXISTS no-op）。

DROP TABLE IF EXISTS {{schema}}.document_attributes;
DROP TABLE IF EXISTS {{schema}}.document_indexes;
DROP TABLE IF EXISTS {{schema}}.document_collections;
DROP TABLE IF EXISTS {{schema}}.document_databases;
