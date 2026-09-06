-- 对称回滚：仅按本库 DROP 扩展。已建 vector 列的表会因类型依赖而失败，
-- 需先 DROP 依赖列/表（各项目业务文档表随项目 schema 删除）。
DROP EXTENSION IF EXISTS vector;
