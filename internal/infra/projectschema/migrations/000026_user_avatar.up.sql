-- 用户头像一等化（graviton-games monsters 资料上云，对外展示字段）：
-- users 增 avatar 列。写入门禁（https URL ≤1024 字节）在 proto validate 注解；
-- 客户端改头像走 PATCH /v1/account（UpdateAccount），管理面走 UpdateUser。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.users
    ADD COLUMN avatar VARCHAR(1024) NOT NULL DEFAULT '';
