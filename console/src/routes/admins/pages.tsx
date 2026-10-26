import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/uikit-core";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { toast } from "sonner";
import { Plus, ShieldCheck } from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { ADMIN_ROLES, getCurrentAdmin, type Admin } from "@/api/admins";
import {
  adminsResource,
  adminsSearch,
  createAdminCommand,
  removeAdminCommand,
  updateAdminCommand,
} from "@/kit/admins";
import { describeApiError } from "@/kit/errors";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS, PAGINATION_ZH } from "@/components/kit/query/pagination-zh";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
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
import { RowDeleteButton } from "@/components/resource/shared";

export function AdminsListPage() {
  const tz = useUserTimezone();
  const [search, setSearch] = useState("");

  // 列表 fleet 化（§12.9-2）：admins 是平台级资源——不随项目作用域；游标/
  // 页大小进 URL（分页栈 = 浏览器历史；旧页无服务端过滤）；页内当前页搜索
  // 保持本地态（旧页语义）。
  const list = useCollection(adminsResource, {
    from: "/console/admins",
    search: adminsSearch,
  });

  const { data: me } = useQuery({
    queryKey: ["console-admin-me"],
    queryFn: getCurrentAdmin,
  });

  const isOwner = me?.role === "owner";

  // 命令接线（§12.3）：toast 单源 onSettled；kit 列表键自动失效（descriptor
  // 挂 resource，平台级无 scope）；存量键无消费方——不桥。
  const remove = useCommand(removeAdminCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("管理员已删除");
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = (a: Admin) => `${a.email} ${a.role}`.toLowerCase();
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((a) => getSearchText(a).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const tableColumns: LegacyColumnDef<Admin, unknown>[] = [
    { id: "email", header: "邮箱", cell: ({ row }) => row.original.email },
    {
      id: "role",
      header: "角色",
      cell: ({ row }) => <AdminRoleBadge role={row.original.role} />,
    },
    {
      id: "created_at",
      header: "创建时间",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    {
      id: "actions",
      header: "操作",
      cell: ({ row }) => (
        <div className="flex items-center justify-end gap-1">
          {isOwner && row.original.id !== me?.id ? (
            <>
              <EditAdminDialog admin={row.original} />
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            </>
          ) : row.original.id === me?.id ? (
            <Badge variant="outline" className="text-xs">
              当前账户
            </Badge>
          ) : null}
        </div>
      ),
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="系统管理员"
        description="管理可登录 Console 的管理员账户（仅 owner 可增删改）"
      />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="当前页内搜索邮箱..."
              className="h-8 w-56"
            />
            {isOwner ? <CreateAdminDialog /> : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} labels={PAGINATION_ZH} />
          </div>
        }
        emptyTitle={search !== "" ? "没有匹配的管理员" : "暂无管理员"}
        emptyDescription={
          search !== ""
            ? "调整搜索词，或清空恢复当前页全部行"
            : "创建第一个管理员后即可协同管理 Console"
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={tableColumns}
            getRowId={(row) => row.id}
          />
        </div>
      </KitListPage>
    </div>
  );
}

function CreateAdminDialog() {
  const [open, setOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState("member");

  // 无表单容器（本地态表单）：rejected/conflict → toast；failure → describeApiError。
  const create = useCommand(createAdminCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("管理员已创建");
        setOpen(false);
        setEmail("");
        setPassword("");
        setRole("member");
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
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
            create.mutate({ email, password, role });
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
            <Button type="submit" disabled={create.pending}>
              {create.pending ? "创建中…" : "创建"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function EditAdminDialog({ admin }: { admin: Admin }) {
  const [open, setOpen] = useState(false);
  const [role, setRole] = useState(admin.role);
  const [password, setPassword] = useState("");

  // diff 在提交处收敛（角色变更/密码非空才传）；无表单容器：结果分支 toast。
  const update = useCommand(updateAdminCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("管理员已更新");
        setOpen(false);
        setPassword("");
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
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
            update.mutate({ id: admin.id, ...input });
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
            <Button type="submit" disabled={update.pending}>
              {update.pending ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
