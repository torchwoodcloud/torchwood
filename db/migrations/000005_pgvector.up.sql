-- pgvector 扩展（会话 #10 §10.5 P0：向量近邻查询；基线重定 2026-09-06：
-- 原 000030 原样保留）。vector 属性类型落地为 pgvector 原生列 VECTOR(dims)，
-- HNSW 索引与 vector_search 一等算子依赖本扩展。镜像侧由
-- pgvector/pgvector:0.8.6-pg18 预装（docker/local + CI service container 同步）；
-- 此处按库启用。
-- 注意（原型 1 实证）：vector 不是 trusted extension，CREATE EXTENSION
-- 需 superuser——当前部署形态的迁移执行身份（compose/CI 的 POSTGRES_USER、
-- testutil admin source）均为 superuser，成立；部署方案中的"扩展安装走
-- superuser 引导步骤" runbook 见 docs/developer/13-operations.md §6.6。
CREATE EXTENSION IF NOT EXISTS vector;
