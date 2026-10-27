import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/uikit-core";
import { predicateOf } from "@lynx-go/uikit-core/schema";
import type { Predicate } from "@lynx-go/uikit-core/kernel";
import { toast } from "sonner";
import { Pencil } from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import {
  getUser,
  deleteUser,
  listUserSessions,
  type User,
  type UserSession,
} from "@/api/users";
import {
  usersResource,
  usersSearch,
  usersCollectionKey,
  userFiltersOf,
  createUserCommand,
  updateUserCommand,
  removeUserCommand,
  resetUserPasswordCommand,
  revokeUserSessionDescriptor,
  issueUserTokenDescriptor,
  type UserFilterField,
} from "@/kit/users";
import { describeApiError } from "@/kit/errors";
import { useAdminRole, canWrite, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue, toDateTimeLocalValue } from "@/lib/datetime";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import {
  FormPageWrapper,
  FormField,
  DetailPageWrapper,
  DetailGrid,
  DetailSkeleton,
  NotFound,
  BulkDeleteButton,
  RowDeleteButton,
  DeleteButton,
} from "@/components/resource/shared";

export function UsersListPage() {
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const writeable = canWrite(role);
  const platformAdmin = isPlatformAdmin(role);

  // 列表 fleet 化（§12.9-2）：游标/页大小/服务端过滤（ID/状态/创建时间
  // 范围）进 URL（分页栈 = 浏览器历史）；页内当前页搜索保持本地态（旧页语义）。
  // scope/enabled 不再逐处传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用
  const list = useCollection(usersResource, {
    from: "/console/users",
    search: usersSearch,
  });

  // 服务端过滤（ParseUserList 白名单 DSL）：ID 精确（受控输入按「查询」提交，
  // 消毒在适配器）+ 状态 + 创建时间范围；setPredicate 即 replace + 清游标
  // 回第一页（旧页手动 reset 的机制化）。创建时间在 URL 存绝对时刻（ISO），
  // 写入边界按管理员时区做墙钟↔绝对换算（audit-logs 同款）。
  const filters = userFiltersOf(list.query.predicate);
  const [idInput, setIdInput] = useState("");
  const setFilters = (next: Partial<Record<UserFilterField, string>>) => {
    const merged = { ...filters, ...next };
    const leaves: Predicate[] = [];
    if (merged.id) leaves.push({ field: "id", op: "eq", value: merged.id });
    if (merged.status) leaves.push({ field: "status", op: "eq", value: merged.status });
    if (merged.created_after)
      leaves.push({ field: "created_after", op: "eq", value: merged.created_after });
    if (merged.created_before)
      leaves.push({ field: "created_before", op: "eq", value: merged.created_before });
    // kit 纯函数归一化（schema/filters）：0 → null、1 → 裸叶、n → AND。
    list.setPredicate(predicateOf(leaves));
  };
  const setCreatedAfter = (local: string) =>
    setFilters({ created_after: fromDateTimeLocalValue(local, tz) || undefined });
  const setCreatedBefore = (local: string) =>
    setFilters({ created_before: fromDateTimeLocalValue(local, tz) || undefined });
  const clearFilters = () => {
    setIdInput("");
    list.setPredicate(null);
  };
  const filtersDirty = list.query.predicate != null;

  // 命令接线（§12.3）：toast 单源 onSettled；失效 = kit 键 + 存量键
  //（resource.legacyKeys 宽前缀 ["users"]——详情/会话页仍吃存量键）。
  const remove = useCommand(removeUserCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("User deleted");
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback(
    (u: User) => `${u.id} ${u.email} ${u.name} ${u.status}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((u) => getSearchText(u).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const toggleRow = (id: string) =>
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const toggleAll = (visibleIds: string[]) =>
    setSelectedIds((prev) =>
      new Set(visibleIds.length > 0 && visibleIds.every((id) => prev.has(id)) ? [] : visibleIds)
    );
  const selectedUsers = list.rows.filter((u) => selectedIds.has(u.id));

  const handleBulkDelete = async () => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selectedUsers.map((u) => deleteUser(u.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`Deletion finished: ${succeeded} succeeded, ${failed} failed`);
      } else {
        toast.success(`Deleted ${selectedUsers.length} users`);
      }
      void queryClient.invalidateQueries({ queryKey: usersCollectionKey });
      setSelectedIds(new Set());
    } finally {
      setBulkDeleting(false);
    }
  };

  const tableColumns: LegacyColumnDef<User, unknown>[] = [
    {
      id: "id",
      header: "ID",
      cell: ({ row }) => (
        <span className="inline-block max-w-[140px] truncate font-mono text-xs">
          {row.original.id}
        </span>
      ),
    },
    { id: "email", header: "Email", cell: ({ row }) => row.original.email },
    { id: "name", header: "Name", cell: ({ row }) => row.original.name },
    {
      id: "status",
      header: "Status",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "active" ? "default" : "secondary"}>
          {row.original.status}
        </Badge>
      ),
    },
    {
      id: "verified",
      header: "Verified",
      cell: ({ row }) => (row.original.email_verified ? "Yes" : "No"),
    },
    {
      id: "created",
      header: "Created",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(writeable || platformAdmin
      ? [
          {
            id: "actions",
            header: "Actions",
            cell: ({ row }: { row: { original: User } }) => (
              <div className="flex items-center justify-end gap-1">
                {writeable ? (
                  <Button variant="ghost" size="icon" asChild>
                    <Link to="/console/users/$id/edit" params={{ id: row.original.id }} title="Edit">
                      <Pencil className="h-4 w-4" />
                    </Link>
                  </Button>
                ) : null}
                {platformAdmin ? (
                  <RowDeleteButton
                    onConfirm={() => remove.mutate({ id: row.original.id })}
                    loading={remove.pending}
                  />
                ) : null}
              </div>
            ),
          } satisfies LegacyColumnDef<User, unknown>,
        ]
      : []),
  ];

  const filtered = list.query.predicate != null;

  return (
    <div className="space-y-6">
      <PageHeader title="Users" description="Users registered in the current project" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="space-y-3 px-4 pb-2 pt-2">
            <div className="flex flex-wrap items-center gap-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Search email, name or ID on this page..."
                className="h-8 w-56"
              />
              {selectedUsers.length > 0 && platformAdmin ? (
                <BulkDeleteButton
                  count={selectedUsers.length}
                  loading={bulkDeleting}
                  onConfirm={() => void handleBulkDelete()}
                />
              ) : null}
              {writeable ? (
                <Button asChild size="sm" className="ml-auto">
                  <Link to="/console/users/new">New User</Link>
                </Button>
              ) : null}
            </div>
            <form
              className="flex flex-wrap items-end gap-3"
              onSubmit={(e) => {
                e.preventDefault();
                setFilters({ id: idInput.trim() || undefined });
              }}
            >
              <div className="space-y-1">
                <Label htmlFor="users-filter-id" className="text-xs text-muted-foreground">
                  User ID
                </Label>
                <Input
                  id="users-filter-id"
                  value={idInput}
                  onChange={(e) => setIdInput(e.target.value)}
                  placeholder="Filter by exact user ID"
                  className="h-8 w-[280px] font-mono text-xs"
                />
              </div>
              <div className="space-y-1">
                <Label className="text-xs text-muted-foreground">Status</Label>
                <Select
                  value={filters.status || "all"}
                  onValueChange={(v) => setFilters({ status: v === "all" ? undefined : v })}
                >
                  <SelectTrigger className="h-8 w-[120px]" aria-label="Status">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">All</SelectItem>
                    <SelectItem value="active">active</SelectItem>
                    <SelectItem value="inactive">inactive</SelectItem>
                    <SelectItem value="blocked">blocked</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="users-filter-after" className="text-xs text-muted-foreground">
                  Created from
                </Label>
                <Input
                  id="users-filter-after"
                  type="datetime-local"
                  value={toDateTimeLocalValue(filters.created_after, tz)}
                  onChange={(e) => setCreatedAfter(e.target.value)}
                  className="h-8 w-[210px]"
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="users-filter-before" className="text-xs text-muted-foreground">
                  To
                </Label>
                <Input
                  id="users-filter-before"
                  type="datetime-local"
                  value={toDateTimeLocalValue(filters.created_before, tz)}
                  onChange={(e) => setCreatedBefore(e.target.value)}
                  className="h-8 w-[210px]"
                />
              </div>
              <Button type="submit" variant="outline" size="sm">
                Apply
              </Button>
              {filtersDirty && (
                <Button type="button" variant="ghost" size="sm" onClick={clearFilters}>
                  Clear
                </Button>
              )}
            </form>
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={
          search !== "" ? "No matching users" : filtered ? "No users match these filters" : "No users yet"
        }
        emptyDescription={
          search !== ""
            ? "Adjust the search term, or clear it to restore all rows on this page"
            : filtered
              ? "Adjust the filters or clear them to see all rows"
              : "Users will appear here once registered"
        }
        emptyAction={
          writeable ? (
            <Button asChild variant="outline" size="sm">
              <Link to="/console/users/new">New User</Link>
            </Button>
          ) : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={tableColumns}
            getRowId={(row) => row.id}
            onRowClick={(u) => navigate({ to: `/console/users/${u.id}` })}
            selection={
              platformAdmin
                ? {
                    selectedIds,
                    onToggle: toggleRow,
                    onToggleAll: toggleAll,
                  }
                : undefined
            }
          />
        </div>
      </KitListPage>
    </div>
  );
}

export function CreateUserPage() {
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [status, setStatus] = useState("active");
  const [labels, setLabels] = useState("");

  // 无表单容器（FormPageWrapper 本地态）：结果分支 toast；failure → describeApiError。
  // labels 逗号串→数组的收敛在命令 run 内（kit/users.ts）。
  const create = useCommand(createUserCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("User created");
        navigate({ to: `/console/users/${result.payload.id}` });
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  return (
    <FormPageWrapper
      title="New User"
      description="Creates the user directly on the server; they can sign in with email + password immediately"
      backTo="/console/users"
      submitLabel="Create"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate({ email, password, name, status, labels });
      }}
      loading={create.pending}
      submitDisabled={!canWrite(role)}
    >
      <FormField id="email" label="Email" value={email} onChange={setEmail} required type="email" />
      <FormField
        id="password"
        label="Password"
        value={password}
        onChange={setPassword}
        required
        type="password"
        placeholder="At least 8 characters, with both letters and digits"
      />
      <FormField id="name" label="Name" value={name} onChange={setName} />
      <div className="space-y-2">
        <Label htmlFor="status">Status</Label>
        <Select value={status} onValueChange={setStatus}>
          <SelectTrigger id="status">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="active">active</SelectItem>
            <SelectItem value="inactive">inactive</SelectItem>
            <SelectItem value="blocked">blocked</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <FormField
        id="labels"
        label="Labels (comma-separated)"
        value={labels}
        onChange={setLabels}
        placeholder="e.g. vip,beta"
      />
    </FormPageWrapper>
  );
}

function SessionsCard({ user }: { user: User }) {
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const writeable = canWrite(role);

  const { data: sessions = [], isLoading } = useQuery({
    queryKey: ["users", user.id, "sessions"],
    queryFn: () => listUserSessions(user.id),
    enabled: !!user.id,
  });

  // 裸 descriptor（不挂 resource）：会话不是 users 集合形态；存量
  // ["users", …] 前缀（含本会话键）由 descriptor 自带 legacyKeys 失效。
  const revoke = useCommand(revokeUserSessionDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Session deleted; the corresponding token is invalidated immediately");
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Sessions</CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : sessions.length === 0 ? (
          <p className="text-sm text-muted-foreground">No sessions yet</p>
        ) : (
          <div className="space-y-2">
            {sessions.map((s: UserSession) => (
              <div
                key={s.id}
                className="flex items-center justify-between gap-4 rounded-md border p-3"
              >
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <Badge variant="secondary">{s.provider}</Badge>
                    <span className="font-mono text-xs text-muted-foreground truncate">
                      {s.id}
                    </span>
                  </div>
                  <div className="mt-1 text-xs text-muted-foreground truncate">
                    {s.ip || "-"} · {s.user_agent || "-"} ·{" "}
                    {s.expire_at ? `Expires at ${formatDateTime(s.expire_at, tz)}` : "No expiration"}
                  </div>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!writeable || revoke.pending}
                  onClick={() => revoke.mutate({ userId: user.id, sessionId: s.id })}
                >
                  Delete
                </Button>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

export function UserDetailPage() {
  const { id } = useParams({ strict: false });
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const [resetOpen, setResetOpen] = useState(false);
  const [newPassword, setNewPassword] = useState("");
  const [tokenOpen, setTokenOpen] = useState(false);
  const [issuedToken, setIssuedToken] = useState<string | null>(null);
  const tz = useUserTimezone();
  const writeable = canWrite(role);
  const platformAdmin = isPlatformAdmin(role);

  const { data: user, isLoading } = useQuery({
    queryKey: ["users", id],
    queryFn: () => getUser(id!),
    enabled: !!id,
  });

  // 命令接线（§12.3）：toast 单源 onSettled；失效 = kit 键 + 存量键
  //（resource.legacyKeys 宽前缀 ["users"]）。
  const remove = useCommand(removeUserCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("User deleted");
        navigate({ to: "/console/users" });
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 对话框内含输入框但无错误行容器 → 结果分支 toast（api-keys 编辑对话框同款）。
  const resetPassword = useCommand(resetUserPasswordCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Password reset; all sessions of this user have been invalidated");
        setResetOpen(false);
        setNewPassword("");
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  // 裸 descriptor：令牌签发不涉 users 集合；payload 一次性展示（reveal 语义）。
  const issueToken = useCommand(issueUserTokenDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        setTokenOpen(false);
        setIssuedToken(result.payload.access_token);
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!user) return <NotFound backTo="/console/users" />;

  return (
    <DetailPageWrapper
      title={user.name || user.email}
      description="User details"
      backTo="/console/users"
      actions={
        <div className="flex gap-2">
          {/* 行为轨迹：Analytics 读方法对 Console admin 全角色开放（viewer 含），
              不做角色 gating（docs/design/analytics.md §4.1/§9）。 */}
          <Button asChild size="sm" variant="outline">
            <Link to="/console/analytics/users/$userId" params={{ userId: encodeURIComponent(user.id) }}>
              Activity
            </Link>
          </Button>
          {platformAdmin && (
            <Button asChild size="sm" variant="outline">
              <Link to="/console/assets/users" search={{ owner: user.id }}>
                User assets
              </Link>
            </Button>
          )}
          {platformAdmin && (
            <Button asChild size="sm" variant="outline">
              <Link to="/console/subscriptions" search={{ q: user.id }}>
                User subscriptions
              </Link>
            </Button>
          )}
          {platformAdmin && (
            <Button size="sm" onClick={() => issueToken.mutate({ userId: user.id })} disabled={issueToken.pending}>
              Impersonate
            </Button>
          )}
          {platformAdmin && (
            <Button size="sm" variant="outline" onClick={() => setResetOpen(true)}>
              Reset password
            </Button>
          )}
          {writeable && (
            <Button asChild variant="outline" size="sm">
              <Link to="/console/users/$id/edit" params={{ id: user.id }}>Edit</Link>
            </Button>
          )}
          {platformAdmin && (
            <DeleteButton onConfirm={() => remove.mutate({ id: user.id })} loading={remove.pending} />
          )}
        </div>
      }
    >
      <div className="space-y-4">
        <DetailGrid
          items={[
            { label: "ID", value: user.id, mono: true },
            { label: "Email", value: user.email },
            { label: "Name", value: user.name },
            { label: "Status", value: user.status },
            { label: "Email verified", value: user.email_verified ? "Yes" : "No" },
            { label: "Phone", value: user.phone || "-" },
            {
              label: "Labels",
              value: user.labels && user.labels.length > 0 ? user.labels.join(", ") : "-",
            },
            {
              label: "Preferences",
              value: user.prefs && Object.keys(user.prefs).length > 0 ? JSON.stringify(user.prefs) : "-",
              mono: true,
            },
            { label: "Created", value: formatDateTime(user.created_at, tz) },
            { label: "Updated", value: formatDateTime(user.updated_at, tz) },
          ]}
        />
        <SessionsCard user={user} />
      </div>

      <ConfirmDialog
        open={resetOpen}
        title="Reset password"
        description="After the reset, all sessions of this user will be invalidated immediately and they will need to sign in again."
        confirmLabel="Reset"
        loading={resetPassword.pending}
        onConfirm={() => resetPassword.mutate({ id: id!, password: newPassword })}
        onOpenChange={(open) => {
          setResetOpen(open);
          if (!open) setNewPassword("");
        }}
      >
        <div className="space-y-2">
          <Label htmlFor="new-password">New password</Label>
          <Input
            id="new-password"
            type="password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
            placeholder="At least 8 characters, with both letters and digits"
          />
        </div>
      </ConfirmDialog>

      <ConfirmDialog
        open={tokenOpen}
        title="Impersonate"
        description="A new session will be created as this user and a token issued (the user will not be notified)."
        confirmLabel="Issue"
        loading={issueToken.pending}
        onConfirm={() => issueToken.mutate({ userId: user.id })}
        onOpenChange={setTokenOpen}
      />

      {issuedToken && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" onClick={() => setIssuedToken(null)}>
          <Card className="w-full max-w-lg" onClick={(e) => e.stopPropagation()}>
            <CardHeader>
              <CardTitle className="text-base">Impersonation token</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              <p className="text-xs text-muted-foreground">
                Use this token as a Bearer token to call the Client API (e.g. GET /v1/account/me). It is valid for the same duration as the access token.
              </p>
              <pre className="max-h-48 overflow-auto rounded-md bg-muted p-3 font-mono text-xs break-all">
                {issuedToken}
              </pre>
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  navigator.clipboard.writeText(issuedToken);
                  toast.success("Copied to clipboard");
                }}
              >
                Copy
              </Button>
              <Button size="sm" onClick={() => setIssuedToken(null)}>
                Close
              </Button>
            </CardContent>
          </Card>
        </div>
      )}
    </DetailPageWrapper>
  );
}

export function UserEditPage() {
  const { id } = useParams({ strict: false });
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const [status, setStatus] = useState<string>("");
  const [name, setName] = useState<string>("");
  const [email, setEmail] = useState<string>("");
  const [emailVerified, setEmailVerified] = useState<boolean | undefined>(undefined);

  const { data: user, isLoading } = useQuery({
    queryKey: ["users", id],
    queryFn: () => getUser(id!),
    enabled: !!id,
  });

  // 无表单容器（FormPageWrapper 本地态）：结果分支 toast；failure → describeApiError。
  const update = useCommand(updateUserCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("User updated");
        // 用响应重建表单，避免与（可能被服务端归一化/回写修改的）服务端状态失同步（R11-P3-11）。
        setStatus(result.payload.status);
        setName(result.payload.name ?? "");
        setEmail(result.payload.email);
        setEmailVerified(result.payload.email_verified);
        navigate({ to: `/console/users/${id}` });
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!user) return <NotFound backTo="/console/users" />;

  const currentStatus = status || user.status;

  return (
    <FormPageWrapper
      title="Edit user"
      description={user.email}
      backTo={`/console/users/${id}`}
      backLabel="Back to details"
      onSubmit={(e) => {
        e.preventDefault();
        update.mutate({
          id: id!,
          status: currentStatus,
          name: name || undefined,
          email: email || undefined,
          email_verified: emailVerified,
        });
      }}
      loading={update.pending}
      submitDisabled={!canWrite(role)}
    >
      <FormField id="name" label="Name" value={name || user.name} onChange={setName} />
      <FormField id="email" label="Email" value={email || user.email} onChange={setEmail} type="email" />
      <div className="space-y-2">
        <Label htmlFor="status">Status</Label>
        <Select value={currentStatus} onValueChange={setStatus}>
          <SelectTrigger id="status">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="active">active</SelectItem>
            <SelectItem value="inactive">inactive</SelectItem>
            <SelectItem value="blocked">blocked</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className="space-y-2">
        <Label htmlFor="email-verified">Email verified</Label>
        <Select
          value={emailVerified === undefined ? String(user.email_verified) : String(emailVerified)}
          onValueChange={(v) => setEmailVerified(v === "true")}
        >
          <SelectTrigger id="email-verified">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="true">Verified</SelectItem>
            <SelectItem value="false">Unverified</SelectItem>
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">This status is reset to unverified automatically after the email is changed.</p>
      </div>
    </FormPageWrapper>
  );
}
