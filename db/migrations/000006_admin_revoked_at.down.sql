-- 对称回滚：撤销列随迁移回收（撤销痕迹清零，回滚窗口内旧行为=仅 Redis 快路径）。
ALTER TABLE admins DROP COLUMN IF EXISTS revoked_at;
