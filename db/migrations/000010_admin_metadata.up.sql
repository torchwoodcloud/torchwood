-- 管理员通用偏好 metadata（JSONB）：存储自选偏好（首键 timezone，IANA 时区名；
-- 键不存在 = 跟随浏览器时区）。对齐项目数据面 users.prefs JSONB 先例——后续
-- 新增偏好键免迁移；API 面仍以 typed 字段投影（Admin.timezone），metadata 本身
-- 不出 proto。写入一律走 repo 的单语句合并（|| / -），避免读-改-写丢键。
ALTER TABLE admins ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;
