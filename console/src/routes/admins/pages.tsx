import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { toast } from "sonner";
import { Plus, ShieldCheck } from "lucide-react";
import {
  ADMIN_ROLES,
  createAdmin,
  deleteAdmin,
  getCurrentAdmin,
  listAdmins,
  updateAdmin,
  type Admin,
} from "@/api/admins";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { AdminRoleBadge } from "@/components/AdminRoleBadge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { RowDeleteButton } from "@/components/resource/shared";
import type { ColumnDef } from "@/components/list/DataTable";

const columns = (tz: string): ColumnDef<Admin>[] => [
  { key: "email", header: "邮箱", cell: (a) => a.email },
  {
    key: "role",
    header: "角色",
    cell: (a) => <AdminRoleBadge role={a.role} />,
  },
  {
    key: "created_at",
    header: "创建时间",
    cell: (a) => formatDateTime(a.created_at, tz),
  },
];

export function AdminsListPage() {
  const queryClient = useQueryClient();
  const tz = useUserTimezone();

  const { data: admins = [], isLoading } = useQuery({
    queryKey: ["console-admins"],
    queryFn: listAdmins,
  });

  const { data: me } = useQuery({
    queryKey: ["console-admin-me"],
    queryFn: getCurrentAdmin,
  });

  const isOwner = me?.role === "owner";

  const remove = useMutation({
    mutationFn: deleteAdmin,
    onSuccess: () => {
      toast.success("管理员已删除");
      queryClient.invalidateQueries({ queryKey: ["console-admins"] });
    },
  });

  return (
    <ResourceListPage
      title="系统管理员"
      description="管理可登录 Console 的管理员账户（仅 owner 可增删改）"
      searchPlaceholder="搜索邮箱..."
      isLoading={isLoading}
      items={admins}
      columns={columns(tz)}
      getSearchText={(a) => `${a.email} ${a.role}`}
      toolbarActions={
        isOwner ? (
          <CreateAdminDialog onCreated={() => queryClient.invalidateQueries({ queryKey: ["console-admins"] })} />
        ) : undefined
      }
      rowActions={(a) => (
        <div className="flex items-center gap-1">
          {isOwner && a.id !== me?.id ? (
            <>
              <EditAdminDialog
                admin={a}
                onSaved={() => queryClient.invalidateQueries({ queryKey: ["console-admins"] })}
              />
              <RowDeleteButton
                onConfirm={() => remove.mutate(a.id)}
                loading={remove.isPending}
              />
            </>
          ) : a.id === me?.id ? (
            <Badge variant="outline" className="text-xs">
              当前账户
            </Badge>
          ) : null}
        </div>
      )}
      emptyTitle="暂无管理员"
      emptyDescription="创建第一个管理员后即可协同管理 Console"
    />
  );
}

function CreateAdminDialog({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState("member");

  const mutation = useMutation({
    mutationFn: createAdmin,
    onSuccess: () => {
      toast.success("管理员已创建");
      setOpen(false);
      setEmail("");
      setPassword("");
      setRole("member");
      onCreated();
    },
  });

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button>
          <Plus className="h-4 w-4 mr-2" />
          新建管理员
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>新建管理员</DialogTitle>
          <DialogDescription>创建可登录 Console 的管理员账户</DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            mutation.mutate({ email, password, role });
          }}
        >
          <div className="space-y-2">
            <Label htmlFor="admin-email">邮箱</Label>
            <Input
              id="admin-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="ops@example.com"
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="admin-password">初始密码</Label>
            <Input
              id="admin-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="至少 8 位，含字母和数字"
              required
            />
          </div>
          <div className="space-y-2">
            <Label>角色</Label>
            <Select value={role} onValueChange={setRole}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {ADMIN_ROLES.map((r) => (
                  <SelectItem key={r} value={r}>
                    {r}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              owner/admin 可查看管理员列表；owner 可增删改管理员。
            </p>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              取消
            </Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "创建中…" : "创建"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function EditAdminDialog({
  admin,
  onSaved,
}: {
  admin: Admin;
  onSaved: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [role, setRole] = useState(admin.role);
  const [password, setPassword] = useState("");

  const mutation = useMutation({
    mutationFn: (input: { role?: string; password?: string }) =>
      updateAdmin(admin.id, input),
    onSuccess: () => {
      toast.success("管理员已更新");
      setOpen(false);
      setPassword("");
      onSaved();
    },
  });

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="ghost" size="icon">
          <ShieldCheck className="h-4 w-4" />
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>编辑管理员</DialogTitle>
          <DialogDescription>{admin.email}</DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            const input: { role?: string; password?: string } = {};
            if (role !== admin.role) input.role = role;
            if (password) input.password = password;
            if (Object.keys(input).length === 0) {
              toast.error("没有需要保存的变更");
              return;
            }
            mutation.mutate(input);
          }}
        >
          <div className="space-y-2">
            <Label>角色</Label>
            <Select value={role} onValueChange={setRole}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {ADMIN_ROLES.map((r) => (
                  <SelectItem key={r} value={r}>
                    {r}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor={`admin-pw-${admin.id}`}>重置密码（留空则不修改）</Label>
            <Input
              id={`admin-pw-${admin.id}`}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="至少 8 位，含字母和数字"
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              取消
            </Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
