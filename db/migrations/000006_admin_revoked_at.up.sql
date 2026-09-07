-- admin 凭证撤销持久层（M5 C1）：admins 加 revoked_at（NULL=未撤销）。
-- 事实源收敛到 DB：改密/删除等管理动作在同事务写 revoked_at，validator 随
-- GetAdmin 行读出判定（iat <= revoked_at 即拒）；Redis revoke 记录保留为
-- 登出快路径，判定取两者 max。此前撤销只存 Redis（TTL 后遗忘），管理面
-- 改密/删除不产生任何撤销痕迹。

ALTER TABLE admins ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ NULL;
