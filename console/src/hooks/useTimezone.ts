import { useQuery } from "@tanstack/react-query";
import { getCurrentAdmin } from "@/api/admins";
import { resolveTimezone } from "@/lib/datetime";

// useUserTimezone 返回当前管理员生效时区：admins/me 的偏好 → 浏览器时区。
// 与 useAdminRole 共享 ["console-admin-me"] 查询缓存——偏好保存后 invalidate
// 该 key，全部消费组件随本次重渲染拿到新时区。
export function useUserTimezone(): string {
  const { data } = useQuery({
    queryKey: ["console-admin-me"],
    queryFn: getCurrentAdmin,
    retry: 1,
    staleTime: 60_000,
  });
  return resolveTimezone(data?.timezone);
}
