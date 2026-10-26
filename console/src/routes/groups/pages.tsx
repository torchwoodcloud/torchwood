import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/uikit-core";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { toast } from "sonner";
import { Plus, Settings2, UserPlus } from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import {
  getGroup,
  deleteGroup,
  listMemberships,
  deleteMembership,
  getGroupPrefs,
  type Group,
  type Membership,
} from "@/api/groups";
import {
  groupsResource,
  groupsSearch,
  groupsCollectionKey,
  createGroupCommand,
  removeGroupCommand,
  updateGroupPrefsCommand,
  addGroupMemberCommand,
  setGroupMemberRoleCommand,
  setGroupMemberStatusCommand,
  removeGroupMemberCommand,
} from "@/kit/groups";
import { describeApiError } from "@/kit/errors";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { useServerPaging } from "@/hooks/useServerPaging";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS, PAGINATION_ZH } from "@/components/kit/query/pagination-zh";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { ColumnDef } from "@/components/list/DataTable";
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

const MEMBERSHIP_ROLES = ["owner", "admin", "member"] as const;

const STATUS_LABELS: Record<string, string> = {
  pending: "待处理",
  accepted: "已通过",
  rejected: "已拒绝",
};

function statusBadge(status: string) {
  const variant =
    status === "accepted" ? "default" : status === "rejected" ? "outline" : "secondary";
  return <Badge variant={variant}>{STATUS_LABELS[status] ?? status}</Badge>;
}

export function GroupsListPage() {
  const { role } = useAdminRole();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const tz = useUserTimezone();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const writeable = canWrite(role);

  // 列表 fleet 化（§12.9-2）：游标/页大小进 URL（分页栈 = 浏览器历史；旧页
  // 无服务端过滤）；页内当前页搜索保持本地态（旧页语义）。
  // scope/enabled 不再逐处传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用
  const list = useCollection(groupsResource, {
    from: "/console/groups",
    search: groupsSearch,
  });

  // 命令接线（§12.3）：toast 单源 onSettled；失效 = kit 键 + 存量键
  //（resource.legacyKeys 宽前缀 ["groups"]/["memberships"]——详情页仍吃存量键）。
  const remove = useCommand(removeGroupCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("用户组已删除");
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback((t: Group) => `${t.id} ${t.name}`.toLowerCase(), []);

  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((t) => getSearchText(t).includes(search.toLowerCase()));
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
  const selectedGroups = list.rows.filter((t) => selectedIds.has(t.id));

  const handleBulkDelete = async () => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selectedGroups.map((t) => deleteGroup(t.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selectedGroups.length} 个用户组`);
      }
      void queryClient.invalidateQueries({ queryKey: groupsCollectionKey });
      setSelectedIds(new Set());
    } finally {
      setBulkDeleting(false);
    }
  };

  const tableColumns: LegacyColumnDef<Group, unknown>[] = [
    {
      id: "id",
      header: "ID",
      cell: ({ row }) => (
        <span className="inline-block max-w-[140px] truncate font-mono text-xs">
          {row.original.id}
        </span>
      ),
    },
    { id: "name", header: "名称", cell: ({ row }) => row.original.name },
    {
      id: "total",
      header: "成员数",
      cell: ({ row }) => String(row.original.total ?? 0),
    },
    {
      id: "created",
      header: "创建时间",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(writeable
      ? [
          {
            id: "actions",
            header: "操作",
              cell: ({ row }: { row: { original: Group } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<Group, unknown>,
        ]
      : []),
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Groups" description="管理项目用户组与成员邀请" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="当前页内搜索用户组名称或 ID..."
              className="h-8 w-56"
            />
            {selectedGroups.length > 0 && writeable ? (
              <BulkDeleteButton
                count={selectedGroups.length}
                loading={bulkDeleting}
                onConfirm={() => void handleBulkDelete()}
              />
            ) : null}
            {writeable ? (
              <Button asChild className="ml-auto">
                <Link to="/console/groups/new">
                  <Plus className="h-4 w-4 mr-2" />
                  新建用户组
                </Link>
              </Button>
            ) : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} labels={PAGINATION_ZH} />
          </div>
        }
        emptyTitle={search !== "" ? "没有匹配的用户组" : "暂无用户组"}
        emptyDescription={
          search !== ""
            ? "调整搜索词，或清空恢复当前页全部行"
            : "创建用户组并邀请成员协作"
        }
        emptyAction={
          writeable ? (
            <Button asChild>
              <Link to="/console/groups/new">新建用户组</Link>
            </Button>
          ) : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={tableColumns}
            getRowId={(row) => row.id}
            onRowClick={(t) => navigate({ to: `/console/groups/${t.id}` })}
            selection={
              writeable
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

export function GroupNewPage() {
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const [name, setName] = useState("");

  // 无表单容器（FormPageWrapper 本地态）：结果分支 toast；failure → describeApiError。
  const create = useCommand(createGroupCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("用户组创建成功");
        navigate({ to: `/console/groups/${result.payload.id}` });
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  return (
    <FormPageWrapper
      title="新建用户组"
      backTo="/console/groups"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate({ name });
      }}
      loading={create.pending}
      submitDisabled={!canWrite(role)}
    >
      <FormField id="name" label="用户组名称" value={name} onChange={setName} required placeholder="Engineering" />
    </FormPageWrapper>
  );
}

function MembershipRoleSelect({
  value,
  onChange,
  id,
}: {
  value: string;
  onChange: (v: string) => void;
  id: string;
}) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger id={id}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {MEMBERSHIP_ROLES.map((role) => (
          <SelectItem key={role} value={role}>
            {role}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

function GroupPrefsCard({ groupId }: { groupId: string }) {
  const { role } = useAdminRole();
  const [prefsText, setPrefsText] = useState("{}");
  const writeable = canWrite(role);

  const { data: prefs, isLoading } = useQuery({
    queryKey: ["groups", groupId, "prefs"],
    queryFn: () => getGroupPrefs(groupId),
    enabled: !!groupId,
  });

  useEffect(() => {
    if (prefs !== undefined) setPrefsText(JSON.stringify(prefs));
  }, [prefs]);

  // 无表单容器（JSON 文本域本地态）：结果分支 toast；failure → describeApiError。
  // 存量 prefs 键（["groups", id, "prefs"]）由 resource.legacyKeys 宽前缀失效。
  const save = useCommand(updateGroupPrefsCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("用户组偏好已保存");
        setPrefsText(JSON.stringify(result.payload));
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  const handleSave = () => {
    let data: unknown;
    try {
      data = JSON.parse(prefsText);
    } catch {
      toast.error("JSON 格式无效");
      return;
    }
    if (data === null || typeof data !== "object" || Array.isArray(data)) {
      toast.error("必须是 JSON 对象，如 {\"theme\":\"dark\"}");
      return;
    }
    save.mutate({ id: groupId, prefs: data as Record<string, unknown> });
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <Settings2 className="h-4 w-4" />
          用户组偏好
        </CardTitle>
        <p className="text-sm text-muted-foreground">
          以 JSON 对象整体替换用户组偏好（如主题、通知开关等），保存后即时生效。
        </p>
      </CardHeader>
      <CardContent>
        <form
          className="space-y-4 max-w-lg"
          onSubmit={(e) => {
            e.preventDefault();
            handleSave();
          }}
        >
          <FormField
            id="group-prefs-json"
            label="prefs (JSON)"
            value={prefsText}
            onChange={setPrefsText}
            placeholder='{"theme":"dark"}'
          />
          <Button type="submit" disabled={!writeable || isLoading || save.pending}>
            {save.pending ? "保存中..." : "保存"}
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

export function GroupDetailPage() {
  const { id: groupId } = useParams({ strict: false });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteName, setInviteName] = useState("");
  const [inviteRole, setInviteRole] = useState<string>("member");
  const [inviteStatus, setInviteStatus] = useState<string>("pending");
  const writeable = canWrite(role);

  const { data: group, isLoading: groupLoading } = useQuery({
    queryKey: ["groups", groupId],
    queryFn: () => getGroup(groupId!),
    enabled: !!groupId,
  });

  const membershipsPaging = useServerPaging();
  const { data: membershipsData, isLoading: membershipsLoading } = useQuery({
    queryKey: ["memberships", groupId, membershipsPaging.pageSize, membershipsPaging.pageToken],
    queryFn: () =>
      listMemberships(groupId!, {
        pageSize: membershipsPaging.pageSize,
        pageToken: membershipsPaging.pageToken,
      }),
    enabled: !!groupId,
    placeholderData: (prev) => prev,
  });
  const memberships = membershipsData?.rows ?? [];

  // 存量键手动桥——仅剩批量移除成员的直调路径消费（非 useCommand 面，
  // 无 descriptor 可挂失效）；命令路径已由 resource.legacyKeys 宽前缀覆盖。
  const invalidateGroup = () => {
    queryClient.invalidateQueries({ queryKey: ["groups", groupId] });
    queryClient.invalidateQueries({ queryKey: ["memberships", groupId] });
  };

  // 命令接线（§12.3）：toast 单源 onSettled；无表单容器 → 结果分支 toast。
  const removeGroup = useCommand(removeGroupCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("用户组已删除");
        navigate({ to: "/console/groups" });
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const invite = useCommand(addGroupMemberCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("成员邀请已创建");
        setInviteEmail("");
        setInviteName("");
        setInviteRole("member");
        setInviteStatus("pending");
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  const removeMembership = useCommand(removeGroupMemberCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("成员已移除");
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const setStatus = useCommand(setGroupMemberStatusCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("成员状态已更新");
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  const setRole = useCommand(setGroupMemberRoleCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("成员角色已更新");
      } else if (failure) {
        toast.error(describeApiError(failure));
      } else if (result) {
        toast.error(result.message);
      }
    },
  });

  const membershipColumns: ColumnDef<Membership>[] = [
    {
      key: "email",
      header: "邮箱 / 用户",
      cell: (m) => m.email || m.user_id || "—",
    },
    { key: "name", header: "名称", cell: (m) => m.name || "—" },
    {
      key: "roles",
      header: "角色",
      cell: (m) =>
        writeable ? (
          <Select
            value={m.roles?.[0] ?? "member"}
            onValueChange={(role) => setRole.mutate({ groupId: groupId!, membershipId: m.id, roles: [role] })}
          >
            <SelectTrigger className="h-8 w-[108px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {MEMBERSHIP_ROLES.map((role) => (
                <SelectItem key={role} value={role}>
                  {role}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        ) : (
          <Badge variant="outline">{m.roles?.[0] ?? "member"}</Badge>
        ),
    },
    {
      key: "status",
      header: "状态",
      cell: (m) =>
        writeable && m.status === "pending" ? (
          <Select
            value={m.status}
            onValueChange={(status) => {
              if (status === "pending") return;
              setStatus.mutate({ groupId: groupId!, membershipId: m.id, status });
            }}
          >
            <SelectTrigger className="h-8 w-[108px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="pending">{STATUS_LABELS.pending}</SelectItem>
              <SelectItem value="accepted">{STATUS_LABELS.accepted}</SelectItem>
              <SelectItem value="rejected">{STATUS_LABELS.rejected}</SelectItem>
            </SelectContent>
          </Select>
        ) : (
          statusBadge(m.status)
        ),
    },
    {
      key: "joined",
      header: "加入时间",
      cell: (m) => formatDateTime(m.joined_at, tz),
    },
  ];

  const getMembershipSearchText = useCallback(
    (m: Membership) => `${m.id} ${m.email} ${m.name} ${m.user_id} ${m.status} ${m.roles?.join(" ")}`,
    []
  );

  const handleBulkDeleteMemberships = async (selected: Membership[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selected.map((m) => deleteMembership(groupId!, m.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`移除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已移除 ${selected.length} 个成员`);
      }
      invalidateGroup();
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  if (groupLoading) return <DetailSkeleton />;
  if (!group) return <NotFound backTo="/console/groups" />;

  return (
    <div className="space-y-6">
      <DetailPageWrapper
        title={group.name}
        description="用户组详情与成员管理"
        backTo="/console/groups"
        actions={
          writeable ? (
            <DeleteButton
              onConfirm={() => removeGroup.mutate({ id: group.id })}
              loading={removeGroup.pending}
            />
          ) : undefined
        }
      >
        <DetailGrid
          items={[
            { label: "ID", value: group.id, mono: true },
            { label: "名称", value: group.name },
            { label: "成员数", value: String(group.total ?? 0) },
            { label: "创建时间", value: formatDateTime(group.created_at, tz) },
            { label: "更新时间", value: formatDateTime(group.updated_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <GroupPrefsCard groupId={group.id} />

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <UserPlus className="h-4 w-4" />
            邀请成员
          </CardTitle>
          <p className="text-sm text-muted-foreground">
            被邀请人需在 Client API 自行接受或拒绝邀请；此处仅用于管理员创建邀请或直接添加成员。
          </p>
        </CardHeader>
        <CardContent>
          <form
            className="grid gap-4 md:grid-cols-2 lg:grid-cols-5 items-end"
            onSubmit={(e) => {
              e.preventDefault();
              if (!inviteEmail.trim()) {
                toast.error("请填写邮箱");
                return;
              }
              invite.mutate({
                groupId: groupId!,
                email: inviteEmail,
                name: inviteName,
                roles: [inviteRole],
                status: inviteStatus,
              });
            }}
          >
            <div className="space-y-2 lg:col-span-2">
              <Label htmlFor="invite-email">邮箱</Label>
              <Input
                id="invite-email"
                type="email"
                value={inviteEmail}
                onChange={(e) => setInviteEmail(e.target.value)}
                placeholder="member@example.com"
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="invite-name">显示名称</Label>
              <Input
                id="invite-name"
                value={inviteName}
                onChange={(e) => setInviteName(e.target.value)}
                placeholder="可选"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="invite-role">角色</Label>
              <MembershipRoleSelect id="invite-role" value={inviteRole} onChange={setInviteRole} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="invite-status">初始状态</Label>
              <Select value={inviteStatus} onValueChange={setInviteStatus}>
                <SelectTrigger id="invite-status">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="pending">{STATUS_LABELS.pending}</SelectItem>
                  <SelectItem value="accepted">{STATUS_LABELS.accepted}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <Button type="submit" disabled={!writeable || invite.pending}>
              {invite.pending ? "提交中..." : "发送邀请"}
            </Button>
          </form>
        </CardContent>
      </Card>

      <ResourceListPage
        title=""
        cardTitle="成员列表"
        searchPlaceholder="当前页内搜索成员邮箱、名称或 ID..."
        isLoading={membershipsLoading}
        items={memberships}
        columns={membershipColumns}
        getSearchText={getMembershipSearchText}
        serverPaging={{
          page: membershipsPaging.page,
          pageSize: membershipsPaging.pageSize,
          hasPrev: membershipsPaging.hasPrev,
          hasNext: !!membershipsData?.nextPageToken,
          onPrev: membershipsPaging.goPrev,
          onNext: () => membershipsPaging.goNext(membershipsData?.nextPageToken),
          onPageSizeChange: membershipsPaging.setPageSize,
        }}
        selectionActions={
          writeable
            ? (selected, clear) => (
                <BulkDeleteButton
                  count={selected.length}
                  loading={bulkDeleting}
                  onConfirm={() => handleBulkDeleteMemberships(selected, clear)}
                />
              )
            : undefined
        }
        rowActions={
          writeable
            ? (m) => (
                <RowDeleteButton
                  onConfirm={() =>
                    removeMembership.mutate({ groupId: groupId!, membershipId: m.id })
                  }
                  loading={removeMembership.pending}
                />
              )
            : undefined
        }
        emptyTitle="暂无成员"
        emptyDescription="使用上方表单邀请成员加入用户组"
      />
    </div>
  );
}
