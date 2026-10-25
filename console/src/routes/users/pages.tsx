import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/ui-kit";
import { predicateOf } from "@lynx-go/ui-kit/schema";
import type { Predicate } from "@lynx-go/ui-kit/kernel";
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
} from "@/fleet/users";
import { describeApiError } from "@/fleet/errors";
import { useAdminRole, canWrite, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue, toDateTimeLocalValue } from "@/lib/datetime";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/fleet/query/data-table";
import { KeysetPagination } from "@/components/fleet/query/keyset-pagination";
import { ListPage as FleetListPage } from "@/components/fleet/query/list-page";
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
        toast.success("用户已删除");
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
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selectedUsers.length} 个用户`);
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
    { id: "email", header: "邮箱", cell: ({ row }) => row.original.email },
    { id: "name", header: "名称", cell: ({ row }) => row.original.name },
    {
      id: "status",
      header: "状态",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "active" ? "default" : "secondary"}>
          {row.original.status}
        </Badge>
      ),
    },
    {
      id: "verified",
      header: "已验证",
      cell: ({ row }) => (row.original.email_verified ? "是" : "否"),
    },
    {
      id: "created",
      header: "创建时间",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(writeable || platformAdmin
      ? [
          {
            id: "actions",
            header: "操作",
            cell: ({ row }: { row: { original: User } }) => (
              <div className="flex items-center justify-end gap-1">
                {writeable ? (
                  <Button variant="ghost" size="icon" asChild>
                    <Link to="/console/users/$id/edit" params={{ id: row.original.id }} title="编辑">
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
      <PageHeader title="Users" description="当前项目的注册用户" />

      <FleetListPage
        list={shownList}
        toolbar={
          <div className="space-y-3 px-4 pb-2 pt-2">
            <div className="flex flex-wrap items-center gap-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="当前页内搜索邮箱、名称或 ID..."
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
                  <Link to="/console/users/new">创建用户</Link>
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
                  用户 ID
                </Label>
                <Input
                  id="users-filter-id"
                  value={idInput}
                  onChange={(e) => setIdInput(e.target.value)}
                  placeholder="按 UserID 精确过滤"
                  className="h-8 w-[280px] font-mono text-xs"
                />
              </div>
              <div className="space-y-1">
                <Label className="text-xs text-muted-foreground">状态</Label>
                <Select
                  value={filters.status || "all"}
                  onValueChange={(v) => setFilters({ status: v === "all" ? undefined : v })}
                >
                  <SelectTrigger className="h-8 w-[120px]" aria-label="状态">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">全部</SelectItem>
                    <SelectItem value="active">active</SelectItem>
                    <SelectItem value="inactive">inactive</SelectItem>
                    <SelectItem value="blocked">blocked</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="users-filter-after" className="text-xs text-muted-foreground">
                  创建时间从
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
                  到
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
                查询
              </Button>
              {filtersDirty && (
                <Button type="button" variant="ghost" size="sm" onClick={clearFilters}>
                  清除
                </Button>
              )}
            </form>
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} />
          </div>
        }
        emptyTitle={
          search !== "" ? "没有匹配的用户" : filtered ? "该过滤条件下暂无用户" : "暂无用户"
        }
        emptyDescription={
          search !== ""
            ? "调整搜索词，或清空恢复当前页全部行"
            : filtered
              ? "调整过滤条件或清除恢复全部"
              : "用户注册后将显示在此"
        }
        emptyAction={
          writeable ? (
            <Button asChild variant="outline" size="sm">
              <Link to="/console/users/new">创建用户</Link>
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
      </FleetListPage>
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
  // labels 逗号串→数组的收敛在命令 run 内（fleet/users.ts）。
  const create = useCommand(createUserCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("用户已创建");
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
      title="创建用户"
      description="服务端直接创建用户，立即可用邮箱+密码登录"
      backTo="/console/users"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate({ email, password, name, status, labels });
      }}
      loading={create.pending}
      submitDisabled={!canWrite(role)}
    >
      <FormField id="email" label="邮箱" value={email} onChange={setEmail} required type="email" />
      <FormField
        id="password"
        label="密码"
        value={password}
        onChange={setPassword}
        required
        type="password"
        placeholder="至少 8 位，含字母和数字"
      />
      <FormField id="name" label="名称" value={name} onChange={setName} />
      <div className="space-y-2">
        <Label htmlFor="status">状态</Label>
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
        label="标签（逗号分隔）"
        value={labels}
        onChange={setLabels}
        placeholder="例如: vip,beta"
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
        toast.success("会话已删除，对应令牌立即失效");
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">会话</CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <p className="text-sm text-muted-foreground">加载中...</p>
        ) : sessions.length === 0 ? (
          <p className="text-sm text-muted-foreground">暂无会话</p>
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
                    {s.expire_at ? `过期于 ${formatDateTime(s.expire_at, tz)}` : "无过期时间"}
                  </div>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!writeable || revoke.pending}
                  onClick={() => revoke.mutate({ userId: user.id, sessionId: s.id })}
                >
                  删除
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
        toast.success("用户已删除");
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
        toast.success("密码已重置，该用户全部会话已失效");
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
      description="用户详情"
      backTo="/console/users"
      actions={
        <div className="flex gap-2">
          {/* 行为轨迹：Analytics 读方法对 Console admin 全角色开放（viewer 含），
              不做角色 gating（docs/design/analytics.md §4.1/§9）。 */}
          <Button asChild size="sm" variant="outline">
            <Link to="/console/analytics/users/$userId" params={{ userId: encodeURIComponent(user.id) }}>
              行为轨迹
            </Link>
          </Button>
          {platformAdmin && (
            <Button asChild size="sm" variant="outline">
              <Link to="/console/assets/users" search={{ owner: user.id }}>
                用户资产
              </Link>
            </Button>
          )}
          {platformAdmin && (
            <Button asChild size="sm" variant="outline">
              <Link to="/console/subscriptions" search={{ q: user.id }}>
                用户订阅
              </Link>
            </Button>
          )}
          {platformAdmin && (
            <Button size="sm" onClick={() => issueToken.mutate({ userId: user.id })} disabled={issueToken.pending}>
              模拟登录
            </Button>
          )}
          {platformAdmin && (
            <Button size="sm" variant="outline" onClick={() => setResetOpen(true)}>
              重置密码
            </Button>
          )}
          {writeable && (
            <Button asChild variant="outline" size="sm">
              <Link to="/console/users/$id/edit" params={{ id: user.id }}>编辑</Link>
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
            { label: "邮箱", value: user.email },
            { label: "名称", value: user.name },
            { label: "状态", value: user.status },
            { label: "邮箱已验证", value: user.email_verified ? "是" : "否" },
            { label: "手机号", value: user.phone || "-" },
            {
              label: "标签",
              value: user.labels && user.labels.length > 0 ? user.labels.join(", ") : "-",
            },
            {
              label: "偏好",
              value: user.prefs && Object.keys(user.prefs).length > 0 ? JSON.stringify(user.prefs) : "-",
              mono: true,
            },
            { label: "创建时间", value: formatDateTime(user.created_at, tz) },
            { label: "更新时间", value: formatDateTime(user.updated_at, tz) },
          ]}
        />
        <SessionsCard user={user} />
      </div>

      <ConfirmDialog
        open={resetOpen}
        title="重置密码"
        description="重置后该用户的所有会话将立即失效，需要重新登录。"
        confirmLabel="重置"
        loading={resetPassword.pending}
        onConfirm={() => resetPassword.mutate({ id: id!, password: newPassword })}
        onOpenChange={(open) => {
          setResetOpen(open);
          if (!open) setNewPassword("");
        }}
      >
        <div className="space-y-2">
          <Label htmlFor="new-password">新密码</Label>
          <Input
            id="new-password"
            type="password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
            placeholder="至少 8 位，含字母和数字"
          />
        </div>
      </ConfirmDialog>

      <ConfirmDialog
        open={tokenOpen}
        title="模拟登录"
        description="将以该用户身份创建一个新会话并签发令牌（用户不会收到通知）。"
        confirmLabel="签发"
        loading={issueToken.pending}
        onConfirm={() => issueToken.mutate({ userId: user.id })}
        onOpenChange={setTokenOpen}
      />

      {issuedToken && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" onClick={() => setIssuedToken(null)}>
          <Card className="w-full max-w-lg" onClick={(e) => e.stopPropagation()}>
            <CardHeader>
              <CardTitle className="text-base">模拟登录令牌</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              <p className="text-xs text-muted-foreground">
                将该令牌作为 Bearer token 调用 Client API（如 GET /v1/account/me）。有效期与 access token 一致。
              </p>
              <pre className="max-h-48 overflow-auto rounded-md bg-muted p-3 font-mono text-xs break-all">
                {issuedToken}
              </pre>
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  navigator.clipboard.writeText(issuedToken);
                  toast.success("已复制到剪贴板");
                }}
              >
                复制
              </Button>
              <Button size="sm" onClick={() => setIssuedToken(null)}>
                关闭
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
        toast.success("用户已更新");
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
      title="编辑用户"
      description={user.email}
      backTo={`/console/users/${id}`}
      backLabel="返回详情"
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
      <FormField id="name" label="名称" value={name || user.name} onChange={setName} />
      <FormField id="email" label="邮箱" value={email || user.email} onChange={setEmail} type="email" />
      <div className="space-y-2">
        <Label htmlFor="status">状态</Label>
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
        <Label htmlFor="email-verified">邮箱已验证</Label>
        <Select
          value={emailVerified === undefined ? String(user.email_verified) : String(emailVerified)}
          onValueChange={(v) => setEmailVerified(v === "true")}
        >
          <SelectTrigger id="email-verified">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="true">已验证</SelectItem>
            <SelectItem value="false">未验证</SelectItem>
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">修改邮箱后该状态会被自动重置为未验证。</p>
      </div>
    </FormPageWrapper>
  );
}
