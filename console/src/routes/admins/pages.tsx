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
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
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
        toast.success("Admin deleted");
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
    { id: "email", header: "Email", cell: ({ row }) => row.original.email },
    {
      id: "role",
      header: "Role",
      cell: ({ row }) => <AdminRoleBadge role={row.original.role} />,
    },
    {
      id: "created_at",
      header: "Created",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    {
      id: "actions",
      header: "Actions",
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
              Current account
            </Badge>
          ) : null}
        </div>
      ),
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Admins"
        description="Manage admin accounts that can sign in to the Console (only owner can create, update or delete)"
      />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search email on this page..."
              className="h-8 w-56"
            />
            {isOwner ? <CreateAdminDialog /> : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching admins" : "No admins yet"}
        emptyDescription={
          search !== ""
            ? "Adjust the search term, or clear it to restore all rows on this page"
            : "Create the first admin to start managing the Console together"
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
        toast.success("Admin created");
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
          New Admin
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New Admin</DialogTitle>
          <DialogDescription>Create an admin account that can sign in to the Console</DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate({ email, password, role });
          }}
        >
          <div className="space-y-2">
            <Label htmlFor="admin-email">Email</Label>
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
            <Label htmlFor="admin-password">Initial password</Label>
            <Input
              id="admin-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="At least 8 characters, with both letters and digits"
              required
            />
          </div>
          <div className="space-y-2">
            <Label>Role</Label>
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
              owner/admin can view the admin list; owner can create, update or delete admins.
            </p>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.pending}>
              {create.pending ? "Creating…" : "Create"}
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
        toast.success("Admin updated");
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
          <DialogTitle>Edit admin</DialogTitle>
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
              toast.error("No changes to save");
              return;
            }
            update.mutate({ id: admin.id, ...input });
          }}
        >
          <div className="space-y-2">
            <Label>Role</Label>
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
            <Label htmlFor={`admin-pw-${admin.id}`}>Reset password (leave empty to keep unchanged)</Label>
            <Input
              id={`admin-pw-${admin.id}`}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="At least 8 characters, with both letters and digits"
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={update.pending}>
              {update.pending ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
