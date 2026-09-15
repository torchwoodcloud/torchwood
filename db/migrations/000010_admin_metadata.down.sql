-- 回滚：删除管理员通用偏好列（键值随列一并丢弃，偏好回到"跟随浏览器"默认）。
ALTER TABLE admins DROP COLUMN IF EXISTS metadata;
