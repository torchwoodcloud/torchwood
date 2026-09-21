-- 回滚 000026：删除 users.avatar 列。

ALTER TABLE {{schema}}.users
    DROP COLUMN IF EXISTS avatar;
