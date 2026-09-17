import { Badge } from "@/components/ui/badge";

// 角色 → 徽章样式（账户资料页与管理员列表共用，避免两处样式漂移）。
const ROLE_STYLE: Record<string, "default" | "secondary" | "outline" | "destructive"> = {
  owner: "default",
  admin: "secondary",
  member: "outline",
  viewer: "outline",
};

export function AdminRoleBadge({ role }: { role: string }) {
  return <Badge variant={ROLE_STYLE[role] ?? "secondary"}>{role}</Badge>;
}
