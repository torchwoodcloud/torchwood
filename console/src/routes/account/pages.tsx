import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCommand } from "@lynx-go/uikit-core";
import { Check, ChevronRight, Globe } from "lucide-react";
import { toast } from "sonner";
import { getCurrentAdmin } from "@/api/admins";
import { setProjectID } from "@/api/client";
import {
  changeCurrentPasswordCommand,
  updateCurrentTimezoneCommand,
} from "@/kit/account";
import { describeApiError } from "@/kit/errors";
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

export function AccountProfilePage() {
  const { data: me, isLoading } = useCurrentAdmin();
  const tz = useUserTimezone();

  if (isLoading) return <DetailSkeleton />;
  if (!me) return <NotFound backTo="/console" />;

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Profile</CardTitle>
          <CardDescription>
            The currently signed-in Console admin account; email and role are managed by the owner. Contact an admin to change them.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <DetailGrid
            items={[
              { label: "Email", value: me.email },
              { label: "Role", value: <AdminRoleBadge role={me.role} /> },
              { label: "ID", value: me.id, mono: true },
              { label: "Created", value: formatDateTime(me.created_at, tz) },
              { label: "Updated", value: formatDateTime(me.updated_at, tz) },
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

  // useCommand（结果两分）：表单有内联错误行 → rejected/conflict 进 setError；
  // failure → describeApiError（失败通道用户文案）。
  const change = useCommand(changeCurrentPasswordCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        // reason 标记抑制登录页的"探测成功自动跳回 console"（见 Login.tsx）。
        setProjectID(null);
        window.location.href = "/console/login?reason=password_changed";
      } else if (result) {
        setError(result.message);
      } else if (failure) {
        setError(describeApiError(failure));
      }
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>Change Password</CardTitle>
        <CardDescription>
          After a successful change, sign-in credentials on all devices will be invalidated and you will need to sign in again.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="max-w-md space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            setError("");
            if (newPassword !== confirmPassword) {
              setError("New passwords do not match");
              return;
            }
            change.mutate({
              current_password: currentPassword,
              new_password: newPassword,
            });
          }}
        >
          <FormField
            id="current-password"
            label="Current password"
            type="password"
            value={currentPassword}
            onChange={setCurrentPassword}
            required
          />
          <FormField
            id="new-password"
            label="New password"
            type="password"
            value={newPassword}
            onChange={setNewPassword}
            required
            hint="At least 8 characters, with both letters and digits"
          />
          <FormField
            id="confirm-password"
            label="Confirm new password"
            type="password"
            value={confirmPassword}
            onChange={setConfirmPassword}
            required
          />
          {error ? <p className="text-sm text-destructive">{error}</p> : null}
          <Button type="submit" disabled={change.pending}>
            {change.pending ? "Submitting…" : "Change Password"}
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

  // useCommand（结果两分）：无表单容器（列表选择器）→ rejected/conflict 走
  // toast；ok 后 setQueryData 刷新 ["console-admin-me"]（现页行为）。
  const save = useCommand(updateCurrentTimezoneCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        queryClient.setQueryData(["console-admin-me"], result.payload);
        toast.success("Preferences saved");
        onOpenChange(false);
        setDraft(null);
        setFilter("");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
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
          <DialogTitle>Change Time Zone</DialogTitle>
          <DialogDescription>
            Time fields will be displayed in the selected time zone (current: {resolveTimezone(current)}).
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <Input
            placeholder="Search time zones, e.g. Shanghai / Tokyo / Berlin…"
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
                Follow browser
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
                No matching time zones
              </div>
            )}
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            disabled={!dirty || save.pending}
            onClick={() => save.mutate({ timezone: value })}
          >
            {save.pending ? "Saving…" : "Save"}
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
          <CardTitle>Preferences</CardTitle>
          <CardDescription>
            Personal settings; click the current value on the right to change it. Changes take effect immediately after saving.
          </CardDescription>
        </CardHeader>
        <CardContent className="border-t p-0">
          <PreferenceItem
            title="Time zone"
            description="All time fields are displayed in this time zone"
            value={me.timezone ? me.timezone : `Follow browser (${browserTimezone()})`}
            onEdit={() => setTzOpen(true)}
          />
        </CardContent>
      </Card>
      <TimezoneDialog open={tzOpen} onOpenChange={setTzOpen} current={me.timezone} />
    </>
  );
}
