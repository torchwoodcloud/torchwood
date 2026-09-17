import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, ChevronRight, Globe } from "lucide-react";
import { toast } from "sonner";
import { getCurrentAdmin, updateCurrentAdmin, type Admin } from "@/api/admins";
import { setProjectID } from "@/api/client";
import { AdminRoleBadge } from "@/components/AdminRoleBadge";
import {
  FormField,
  DetailGrid,
  DetailSkeleton,
  NotFound,
} from "@/components/resource/shared";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
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
// 查询缓存；资料保存后 setQueryData 该 key，消费组件随重渲染拿到新值。
function useCurrentAdmin() {
  return useQuery({
    queryKey: ["console-admin-me"],
    queryFn: getCurrentAdmin,
    retry: 1,
    staleTime: 60_000,
  });
}

function apiErrorMessage(err: unknown): string | undefined {
  return (
    err as { response?: { data?: { error?: { message?: string } } } }
  )?.response?.data?.error?.message;
}

export function AccountProfilePage() {
  const { data: me, isLoading } = useCurrentAdmin();
  const tz = useUserTimezone();

  if (isLoading) return <DetailSkeleton />;
  if (!me) return <NotFound backTo="/console" />;

  return (
    <div className="space-y-6">
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
      <ChangePasswordCard />
    </div>
  );
}

// ChangePasswordCard 自助改密：须提供当前密码校验，成功后服务端撤销全部
// 凭证（改密前签发的 access/refresh 一律失效），因此强制回登录页重新登录。
function ChangePasswordCard() {
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState("");

  const change = useMutation({
    mutationFn: () =>
      updateCurrentAdmin(
        { current_password: currentPassword, new_password: newPassword },
        { __skipToast: true }
      ),
    onSuccess: () => {
      // reason 标记抑制登录页的"探测成功自动跳回 console"（见 Login.tsx）。
      setProjectID(null);
      window.location.href = "/console/login?reason=password_changed";
    },
    onError: (err) => setError(apiErrorMessage(err) ?? "修改失败，请重试"),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>修改密码</CardTitle>
        <CardDescription>
          修改成功后所有设备的登录凭证将失效，需要重新登录。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="max-w-md space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            setError("");
            if (newPassword !== confirmPassword) {
              setError("两次输入的新密码不一致");
              return;
            }
            change.mutate();
          }}
        >
          <FormField
            id="current-password"
            label="当前密码"
            type="password"
            value={currentPassword}
            onChange={setCurrentPassword}
            required
          />
          <FormField
            id="new-password"
            label="新密码"
            type="password"
            value={newPassword}
            onChange={setNewPassword}
            required
            hint="至少 8 位，且同时包含字母和数字"
          />
          <FormField
            id="confirm-password"
            label="确认新密码"
            type="password"
            value={confirmPassword}
            onChange={setConfirmPassword}
            required
          />
          {error ? <p className="text-sm text-destructive">{error}</p> : null}
          <Button type="submit" disabled={change.isPending}>
            {change.isPending ? "提交中…" : "修改密码"}
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

// PreferenceItem 偏好列表行（为后续配置项留位）：左侧标题/说明，
// 右侧当前值按钮 = 修改入口（点击弹出该项的编辑框）。
function PreferenceItem({
  title,
  description,
  value,
  onEdit,
}: {
  title: string;
  description: string;
  value: string;
  onEdit: () => void;
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-6 py-4">
      <div className="min-w-0">
        <p className="text-sm font-medium">{title}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{description}</p>
      </div>
      <Button variant="outline" size="sm" onClick={onEdit} className="max-w-full">
        <span className="truncate font-mono text-xs">{value}</span>
        <ChevronRight className="ml-1.5 h-4 w-4 shrink-0" />
      </Button>
    </div>
  );
}

// TimezoneDialog 可搜索时区列表：选中只改草稿，显式「保存」才提交；
// "" = 跟随浏览器（清除偏好，回退浏览器时区）。
function TimezoneDialog({
  open,
  onOpenChange,
  current,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  current?: string;
}) {
  const queryClient = useQueryClient();
  const browserTz = browserTimezone();
  const [draft, setDraft] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const value = draft ?? current ?? "";

  const timezones = useMemo(() => {
    const q = filter.trim().toLowerCase();
    const all = supportedTimezones();
    if (!q) return all;
    return all.filter((tz) => tz.toLowerCase().includes(q));
  }, [filter]);

  const save = useMutation({
    mutationFn: (timezone: string) =>
      updateCurrentAdmin({ timezone }, { __skipToast: true }),
    onSuccess: (admin: Admin) => {
      queryClient.setQueryData(["console-admin-me"], admin);
      toast.success("偏好已保存");
      onOpenChange(false);
      setDraft(null);
      setFilter("");
    },
    onError: (err) => toast.error(apiErrorMessage(err) ?? "保存失败，请重试"),
  });

  const dirty = value !== (current ?? "");

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o);
        if (!o) {
          setDraft(null);
          setFilter("");
        }
      }}
    >
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>修改时区</DialogTitle>
          <DialogDescription>
            时间字段将按所选时区显示（当前生效：{resolveTimezone(current)}）。
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <Input
            placeholder="搜索时区，如 Shanghai / Tokyo / Berlin…"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
          <div className="h-72 overflow-y-auto rounded-md border">
            <button
              type="button"
              onClick={() => setDraft("")}
              className={cn(
                "flex w-full items-center justify-between px-3 py-2 text-sm",
                value === ""
                  ? "bg-primary/10 text-primary"
                  : "hover:bg-muted"
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
                onClick={() => setDraft(tz)}
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
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button
            disabled={!dirty || save.isPending}
            onClick={() => save.mutate(value)}
          >
            {save.isPending ? "保存中…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function AccountPreferencesPage() {
  const { data: me, isLoading } = useCurrentAdmin();
  const [tzOpen, setTzOpen] = useState(false);

  if (isLoading) return <DetailSkeleton />;
  if (!me) return <NotFound backTo="/console" />;

  return (
    <>
      <Card>
        <CardHeader>
          <CardTitle>偏好</CardTitle>
          <CardDescription>
            个人配置项；点击右侧当前值修改，保存后立即生效。
          </CardDescription>
        </CardHeader>
        <CardContent className="border-t p-0">
          <PreferenceItem
            title="时区"
            description="所有时间字段按此时区显示"
            value={me.timezone ? me.timezone : `跟随浏览器（${browserTimezone()}）`}
            onEdit={() => setTzOpen(true)}
          />
        </CardContent>
      </Card>
      <TimezoneDialog open={tzOpen} onOpenChange={setTzOpen} current={me.timezone} />
    </>
  );
}
