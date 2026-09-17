import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Globe } from "lucide-react";
import { toast } from "sonner";
import { getCurrentAdmin, updateCurrentAdmin, type Admin } from "@/api/admins";
import { AdminRoleBadge } from "@/components/AdminRoleBadge";
import { DetailGrid, DetailSkeleton, NotFound } from "@/components/resource/shared";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { useUserTimezone } from "@/hooks/useTimezone";
import {
  browserTimezone,
  formatDateTime,
  resolveTimezone,
  supportedTimezones,
} from "@/lib/datetime";
import { cn } from "@/lib/utils";

// useCurrentAdmin 与 useUserTimezone / useAdminRole 共享 ["console-admin-me"]
// 查询缓存；偏好保存后 setQueryData 该 key，消费组件随重渲染拿到新值。
function useCurrentAdmin() {
  return useQuery({
    queryKey: ["console-admin-me"],
    queryFn: getCurrentAdmin,
    retry: 1,
    staleTime: 60_000,
  });
}

export function AccountProfilePage() {
  const { data: me, isLoading } = useCurrentAdmin();
  const tz = useUserTimezone();

  if (isLoading) return <DetailSkeleton />;
  if (!me) return <NotFound backTo="/console" />;

  return (
    <Card>
      <CardHeader>
        <CardTitle>资料</CardTitle>
        <CardDescription>
          当前登录的 Console 管理员账户；邮箱与角色由 owner 管理，如需修改请联系管理员。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <DetailGrid
          items={[
            { label: "邮箱", value: me.email },
            { label: "角色", value: <AdminRoleBadge role={me.role} /> },
            { label: "ID", value: me.id, mono: true },
            { label: "创建时间", value: formatDateTime(me.created_at, tz) },
            { label: "更新时间", value: formatDateTime(me.updated_at, tz) },
          ]}
        />
      </CardContent>
    </Card>
  );
}

// TimezoneSelector 可搜索时区列表（原 PreferencesDialog 迁移到账户设置页）。
// value 为草稿值："" = 跟随浏览器（清除偏好），非空 = IANA 名。
function TimezoneSelector({
  value,
  onChange,
}: {
  value: string;
  onChange: (v: string) => void;
}) {
  const [filter, setFilter] = useState("");
  const browserTz = browserTimezone();
  const timezones = useMemo(() => {
    const q = filter.trim().toLowerCase();
    const all = supportedTimezones();
    if (!q) return all;
    return all.filter((tz) => tz.toLowerCase().includes(q));
  }, [filter]);

  return (
    <div className="max-w-2xl space-y-3">
      <Input
        placeholder="搜索时区，如 Shanghai / Tokyo / Berlin…"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
      />
      <div className="h-72 overflow-y-auto rounded-md border">
        <button
          type="button"
          onClick={() => onChange("")}
          className={cn(
            "flex w-full items-center justify-between px-3 py-2 text-sm",
            value === "" ? "bg-primary/10 text-primary" : "hover:bg-muted"
          )}
        >
          <span className="flex items-center gap-2">
            <Globe className="h-4 w-4" />
            跟随浏览器
            <span className="text-xs text-muted-foreground">{browserTz}</span>
          </span>
          {value === "" && <Check className="h-4 w-4" />}
        </button>
        <div className="border-t" />
        {timezones.map((tz) => (
          <button
            key={tz}
            type="button"
            onClick={() => onChange(tz)}
            className={cn(
              "flex w-full items-center justify-between px-3 py-2 text-sm",
              value === tz ? "bg-primary/10 text-primary" : "hover:bg-muted"
            )}
          >
            <span>{tz}</span>
            {value === tz && <Check className="h-4 w-4" />}
          </button>
        ))}
        {timezones.length === 0 && (
          <div className="px-3 py-6 text-center text-sm text-muted-foreground">
            无匹配时区
          </div>
        )}
      </div>
    </div>
  );
}

export function AccountPreferencesPage() {
  const queryClient = useQueryClient();
  const { data: me, isLoading } = useCurrentAdmin();
  const current = me?.timezone;
  // draft：null = 未改动（跟随当前偏好）；"" = 跟随浏览器；非空 = IANA 名。
  const [draft, setDraft] = useState<string | null>(null);
  const value = draft ?? current ?? "";

  const save = useMutation({
    mutationFn: (timezone: string) => updateCurrentAdmin({ timezone }),
    onSuccess: (admin: Admin) => {
      queryClient.setQueryData(["console-admin-me"], admin);
      // 时区已全局生效（消费组件都挂在 console-admin-me 上），无需额外失效。
      toast.success("偏好已保存");
      setDraft(null);
    },
    onError: () => toast.error("保存失败，请重试"),
  });

  if (isLoading) return <DetailSkeleton />;
  if (!me) return <NotFound backTo="/console" />;

  const dirty = value !== (current ?? "");

  return (
    <Card>
      <CardHeader>
        <CardTitle>偏好</CardTitle>
        <CardDescription>
          时间字段将按所选时区显示（当前生效：{resolveTimezone(current)}）。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <TimezoneSelector value={value} onChange={setDraft} />
        <Button
          disabled={!dirty || save.isPending}
          onClick={() => save.mutate(value)}
        >
          {save.isPending ? "保存中…" : "保存"}
        </Button>
      </CardContent>
    </Card>
  );
}
