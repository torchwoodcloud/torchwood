import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { toast } from "sonner";
import { Plus, Settings2, UserPlus } from "lucide-react";
import {
  listGroups,
  getGroup,
  createGroup,
  deleteGroup,
  listMemberships,
  createMembership,
  updateMembership,
  updateMembershipStatus,
  deleteMembership,
  getGroupPrefs,
  updateGroupPrefs,
  type Group,
  type Membership,
} from "@/api/groups";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { useServerPaging } from "@/hooks/useServerPaging";
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

const groupColumns = (tz: string): ColumnDef<Group>[] => [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[140px] truncate",
    cell: (t) => t.id,
  },
  { key: "name", header: "名称", cell: (t) => t.name },
  { key: "total", header: "成员数", cell: (t) => String(t.total ?? 0) },
  {
    key: "created",
    header: "创建时间",
    cell: (t) => formatDateTime(t.created_at, tz),
  },
];

export function GroupsListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const queryClient = useQueryClient();
  const tz = useUserTimezone();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const writeable = canWrite(role);

  const paging = useServerPaging();
  const { data, isLoading } = useQuery({
    queryKey: ["groups", projectId, paging.pageSize, paging.pageToken],
    queryFn: () => listGroups({ pageSize: paging.pageSize, pageToken: paging.pageToken }),
    enabled: !!projectId,
    placeholderData: (prev) => prev,
  });
  const groups = data?.rows ?? [];

  const remove = useMutation({
    mutationFn: (id: string) => deleteGroup(id),
    onSuccess: () => {
      toast.success("用户组已删除");
      queryClient.invalidateQueries({ queryKey: ["groups", projectId] });
    },
  });

  const getSearchText = useCallback((t: Group) => `${t.id} ${t.name}`, []);

  const handleBulkDelete = async (selected: Group[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selected.map((t) => deleteGroup(t.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selected.length} 个用户组`);
      }
      queryClient.invalidateQueries({ queryKey: ["groups", projectId] });
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  return (
    <ResourceListPage
      title="Groups"
      description="管理项目用户组与成员邀请"
      searchPlaceholder="当前页内搜索用户组名称或 ID..."
      isLoading={isLoading}
      items={groups}
      columns={groupColumns(tz)}
      getSearchText={getSearchText}
      serverPaging={{
        page: paging.page,
        pageSize: paging.pageSize,
        hasPrev: paging.hasPrev,
        hasNext: !!data?.nextPageToken,
        onPrev: paging.goPrev,
        onNext: () => paging.goNext(data?.nextPageToken),
        onPageSizeChange: paging.setPageSize,
      }}
      detailPath={(t) => `/console/groups/${t.id}`}
      toolbarActions={
        writeable ? (
          <Button asChild>
            <Link to="/console/groups/new">
              <Plus className="h-4 w-4 mr-2" />
              新建用户组
            </Link>
          </Button>
        ) : undefined
      }
      selectionActions={
        writeable
          ? (selected, clear) => (
              <BulkDeleteButton
                count={selected.length}
                loading={bulkDeleting}
                onConfirm={() => handleBulkDelete(selected, clear)}
              />
            )
          : undefined
      }
      rowActions={
        writeable
          ? (t) => (
              <RowDeleteButton onConfirm={() => remove.mutate(t.id)} loading={remove.isPending} />
            )
          : undefined
      }
      emptyTitle="暂无用户组"
      emptyDescription="创建用户组并邀请成员协作"
      emptyAction={
        writeable ? (
          <Button asChild>
            <Link to="/console/groups/new">新建用户组</Link>
          </Button>
        ) : undefined
      }
    />
  );
}

export function GroupNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [name, setName] = useState("");

  const mutation = useMutation({
    mutationFn: createGroup,
    onSuccess: (group) => {
      toast.success("用户组创建成功");
      queryClient.invalidateQueries({ queryKey: ["groups", projectId] });
      navigate(`/console/groups/${group.id}`);
    },
  });

  return (
    <FormPageWrapper
      title="新建用户组"
      backTo="/console/groups"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        mutation.mutate({ name });
      }}
      loading={mutation.isPending}
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
  const queryClient = useQueryClient();
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

  const save = useMutation({
    mutationFn: (data: Record<string, unknown>) => updateGroupPrefs(groupId, data),
    onSuccess: (updated) => {
      toast.success("用户组偏好已保存");
      setPrefsText(JSON.stringify(updated));
      queryClient.invalidateQueries({ queryKey: ["groups", groupId, "prefs"] });
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
    save.mutate(data as Record<string, unknown>);
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
          <Button type="submit" disabled={!writeable || isLoading || save.isPending}>
            {save.isPending ? "保存中..." : "保存"}
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

export function GroupDetailPage() {
  const { id: groupId } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
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

  const invalidateGroup = () => {
    queryClient.invalidateQueries({ queryKey: ["groups", groupId] });
    queryClient.invalidateQueries({ queryKey: ["groups", projectId] });
    queryClient.invalidateQueries({ queryKey: ["memberships", groupId] });
  };

  const removeGroup = useMutation({
    mutationFn: (id: string) => deleteGroup(id),
    onSuccess: () => {
      toast.success("用户组已删除");
      queryClient.invalidateQueries({ queryKey: ["groups", projectId] });
      navigate("/console/groups");
    },
  });

  const invite = useMutation({
    mutationFn: () =>
      createMembership(groupId!, {
        email: inviteEmail,
        name: inviteName,
        roles: [inviteRole],
        status: inviteStatus,
      }),
    onSuccess: () => {
      toast.success("成员邀请已创建");
      setInviteEmail("");
      setInviteName("");
      setInviteRole("member");
      setInviteStatus("pending");
      invalidateGroup();
    },
  });

  const removeMembership = useMutation({
    mutationFn: (membershipId: string) => deleteMembership(groupId!, membershipId),
    onSuccess: () => {
      toast.success("成员已移除");
      invalidateGroup();
    },
  });

  const setStatus = useMutation({
    mutationFn: ({ membershipId, status }: { membershipId: string; status: string }) =>
      updateMembershipStatus(groupId!, membershipId, status),
    onSuccess: () => {
      toast.success("成员状态已更新");
      invalidateGroup();
    },
  });

  const setRole = useMutation({
    mutationFn: ({ membershipId, roles }: { membershipId: string; roles: string[] }) =>
      updateMembership(groupId!, membershipId, { roles }),
    onSuccess: () => {
      toast.success("成员角色已更新");
      invalidateGroup();
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
            onValueChange={(role) => setRole.mutate({ membershipId: m.id, roles: [role] })}
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
              setStatus.mutate({ membershipId: m.id, status });
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
              onConfirm={() => removeGroup.mutate(group.id)}
              loading={removeGroup.isPending}
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
              invite.mutate();
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
            <Button type="submit" disabled={!writeable || invite.isPending}>
              {invite.isPending ? "提交中..." : "发送邀请"}
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
                  onConfirm={() => removeMembership.mutate(m.id)}
                  loading={removeMembership.isPending}
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
