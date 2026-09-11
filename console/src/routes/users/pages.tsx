import { useCallback, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  listUsers,
  getUser,
  createUser,
  updateUser,
  updateUserPassword,
  deleteUser,
  listUserSessions,
  deleteUserSession,
  createUserToken,
  type User,
  type UserSession,
} from "@/api/users";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite, isPlatformAdmin } from "@/hooks/useAdminRole";
import { ResourceListPage } from "@/components/list/ResourceListPage";
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

const columns: ColumnDef<User>[] = [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[140px] truncate",
    cell: (u) => u.id,
  },
  { key: "email", header: "邮箱", cell: (u) => u.email },
  { key: "name", header: "名称", cell: (u) => u.name },
  {
    key: "status",
    header: "状态",
    cell: (u) => (
      <Badge variant={u.status === "active" ? "default" : "secondary"}>{u.status}</Badge>
    ),
  },
  { key: "verified", header: "已验证", cell: (u) => (u.email_verified ? "是" : "否") },
  {
    key: "created",
    header: "创建时间",
    cell: (u) => new Date(u.created_at).toLocaleString(),
  },
];

export function UsersListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const queryClient = useQueryClient();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const writeable = canWrite(role);
  const platformAdmin = isPlatformAdmin(role);

  const { data: users = [], isLoading } = useQuery({
    queryKey: ["users", projectId],
    queryFn: listUsers,
    enabled: !!projectId,
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteUser(id),
    onSuccess: () => {
      toast.success("用户已删除");
      queryClient.invalidateQueries({ queryKey: ["users", projectId] });
    },
  });

  const getSearchText = useCallback(
    (u: User) => `${u.id} ${u.email} ${u.name} ${u.status}`,
    []
  );

  const handleBulkDelete = async (selected: User[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selected.map((u) => deleteUser(u.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selected.length} 个用户`);
      }
      queryClient.invalidateQueries({ queryKey: ["users", projectId] });
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  return (
    <ResourceListPage
      title="Users"
      description="当前项目的注册用户"
      searchPlaceholder="搜索邮箱、名称或 ID..."
      isLoading={isLoading}
      items={users}
      columns={columns}
      getSearchText={getSearchText}
      detailPath={(u) => `/console/users/${u.id}`}
      editPath={writeable ? (u) => `/console/users/${u.id}/edit` : undefined}
      toolbarActions={
        writeable ? (
          <Button asChild size="sm">
            <Link to="/console/users/new">创建用户</Link>
          </Button>
        ) : undefined
      }
      selectionActions={
        platformAdmin
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
        platformAdmin
          ? (u) => (
              <RowDeleteButton onConfirm={() => remove.mutate(u.id)} loading={remove.isPending} />
            )
          : undefined
      }
      emptyTitle="暂无用户"
      emptyDescription="用户注册后将显示在此"
      emptyAction={
        writeable ? (
          <Button asChild variant="outline" size="sm">
            <Link to="/console/users/new">创建用户</Link>
          </Button>
        ) : undefined
      }
    />
  );
}

export function CreateUserPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [status, setStatus] = useState("active");
  const [labels, setLabels] = useState("");

  const mutation = useMutation({
    mutationFn: () =>
      createUser({
        email,
        password,
        name,
        status,
        labels: labels
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
      }),
    onSuccess: (user) => {
      toast.success("用户已创建");
      queryClient.invalidateQueries({ queryKey: ["users", projectId] });
      navigate(`/console/users/${user.id}`);
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
        mutation.mutate();
      }}
      loading={mutation.isPending}
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
  const queryClient = useQueryClient();
  const { role } = useAdminRole();
  const writeable = canWrite(role);

  const { data: sessions = [], isLoading } = useQuery({
    queryKey: ["users", user.id, "sessions"],
    queryFn: () => listUserSessions(user.id),
    enabled: !!user.id,
  });

  const revoke = useMutation({
    mutationFn: (sessionId: string) => deleteUserSession(user.id, sessionId),
    onSuccess: () => {
      toast.success("会话已删除，对应令牌立即失效");
      queryClient.invalidateQueries({ queryKey: ["users", user.id, "sessions"] });
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
                    {s.expire_at ? `过期于 ${new Date(s.expire_at).toLocaleString()}` : "无过期时间"}
                  </div>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!writeable || revoke.isPending}
                  onClick={() => revoke.mutate(s.id)}
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
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [resetOpen, setResetOpen] = useState(false);
  const [newPassword, setNewPassword] = useState("");
  const [tokenOpen, setTokenOpen] = useState(false);
  const [issuedToken, setIssuedToken] = useState<string | null>(null);
  const writeable = canWrite(role);
  const platformAdmin = isPlatformAdmin(role);

  const { data: user, isLoading } = useQuery({
    queryKey: ["users", id],
    queryFn: () => getUser(id!),
    enabled: !!id,
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteUser(id),
    onSuccess: () => {
      toast.success("用户已删除");
      queryClient.invalidateQueries({ queryKey: ["users", projectId] });
      navigate("/console/users");
    },
  });

  const resetPassword = useMutation({
    mutationFn: (password: string) => updateUserPassword(id!, password),
    onSuccess: () => {
      toast.success("密码已重置，该用户全部会话已失效");
      setResetOpen(false);
      setNewPassword("");
      queryClient.invalidateQueries({ queryKey: ["users", projectId] });
    },
  });

  const issueToken = useMutation({
    mutationFn: createUserToken,
    onSuccess: (tokens) => {
      setTokenOpen(false);
      setIssuedToken(tokens.access_token);
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
            <Link to={`/console/analytics/users/${encodeURIComponent(user.id)}`}>
              行为轨迹
            </Link>
          </Button>
          {platformAdmin && (
            <Button asChild size="sm" variant="outline">
              <Link to={`/console/assets/users?owner=${encodeURIComponent(user.id)}`}>
                用户资产
              </Link>
            </Button>
          )}
          {platformAdmin && (
            <Button asChild size="sm" variant="outline">
              <Link to={`/console/subscriptions?q=${encodeURIComponent(user.id)}`}>
                用户订阅
              </Link>
            </Button>
          )}
          {platformAdmin && (
            <Button size="sm" onClick={() => issueToken.mutate(user.id)} disabled={issueToken.isPending}>
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
              <Link to={`/console/users/${user.id}/edit`}>编辑</Link>
            </Button>
          )}
          {platformAdmin && (
            <DeleteButton onConfirm={() => remove.mutate(user.id)} loading={remove.isPending} />
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
            { label: "创建时间", value: new Date(user.created_at).toLocaleString() },
            { label: "更新时间", value: new Date(user.updated_at).toLocaleString() },
          ]}
        />
        <SessionsCard user={user} />
      </div>

      <ConfirmDialog
        open={resetOpen}
        title="重置密码"
        description="重置后该用户的所有会话将立即失效，需要重新登录。"
        confirmLabel="重置"
        loading={resetPassword.isPending}
        onConfirm={() => resetPassword.mutate(newPassword)}
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
        loading={issueToken.isPending}
        onConfirm={() => issueToken.mutate(user.id)}
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
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
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

  const mutation = useMutation({
    mutationFn: (input: {
      status: string;
      name?: string;
      email?: string;
      email_verified?: boolean;
    }) => updateUser(id!, input),
    onSuccess: (updated) => {
      toast.success("用户已更新");
      // 用响应重建表单，避免与（可能被服务端归一化/回写修改的）服务端状态失同步（R11-P3-11）。
      setStatus(updated.status);
      setName(updated.name ?? "");
      setEmail(updated.email);
      setEmailVerified(updated.email_verified);
      queryClient.invalidateQueries({ queryKey: ["users", projectId] });
      navigate(`/console/users/${id}`);
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
        mutation.mutate({
          status: currentStatus,
          name: name || undefined,
          email: email || undefined,
          email_verified: emailVerified,
        });
      }}
      loading={mutation.isPending}
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
