import { useMemo, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Check, Globe } from "lucide-react";
import { updateCurrentAdmin, type Admin } from "@/api/admins";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import {
  browserTimezone,
  resolveTimezone,
  supportedTimezones,
} from "@/lib/datetime";
import { cn } from "@/lib/utils";
import { toast } from "sonner";

// 偏好对话框：当前仅时区。跟随浏览器 = 不设偏好（服务端 metadata 无 timezone 键）。
export function PreferencesDialog({
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
  // draft："" = 跟随浏览器；非空 = IANA 名。打开时从当前偏好同步。
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
    mutationFn: (timezone: string) => updateCurrentAdmin({ timezone }),
    onSuccess: (admin: Admin) => {
      queryClient.setQueryData(["console-admin-me"], admin);
      // 时区已全局生效（消费组件都挂在 console-admin-me 上），无需额外失效。
      toast.success("偏好已保存");
      onOpenChange(false);
      setDraft(null);
      setFilter("");
    },
    onError: () => toast.error("保存失败，请重试"),
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
          <DialogTitle>偏好设置</DialogTitle>
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
            <div className="max-h-56 overflow-y-auto">
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
