-- 000033 逆序回滚：恢复 B15 之前的授权形态——运行账号（tw_authenticator）
-- 重新持 public.tw_secrets 四权（SELECT,INSERT,UPDATE,DELETE），对应"密钥
-- 落库挂在运行时启动钩子"旧形态（RolesSigKeySyncHook 时代的引导 SQL ⑤，
-- 13-operations §4.5 历史授权面）。回滚目标是配合 B15 之前的二进制（启动
-- 钩子以运行 DSN 落库）；PUBLIC 与三角色（tw_owner/tw_app/tw_system）自
-- 000029 起即无授权，无需恢复。
--
-- 与 up 对称的 DO 块守护：tw_authenticator 不存在（未引导的库/测试库）时
-- 跳过，保持零权限形态（幂等）。

DO $do$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tw_authenticator') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON public.tw_secrets TO tw_authenticator;
    END IF;
END $do$;
