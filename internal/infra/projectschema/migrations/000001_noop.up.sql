-- 每项目 catalog 四表已退役（阶段②包 A，redesign §4.2/C1）：catalog 全局化
-- 至 public.catalog_databases / catalog_collections（db/migrations/000003），
-- 本模板不再创建；存量四表由 000011 清理。POC 测试库重建，无数据搬迁。
-- 版本号占位保留（schema_migrations 记录语义依赖单调版本序），SELECT 1 是
-- 合法 no-op 迁移体。

SELECT 1;
