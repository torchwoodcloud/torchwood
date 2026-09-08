import { useCallback, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Copy, Plus, Trash2 } from "lucide-react";
import {
  listProjects,
  getProject,
  createProject,
  updateProject,
  deleteProject,
  listInviteCodes,
  createInviteCode,
  deleteInviteCode,
  type Project,
  type InviteCode,
} from "@/api/projects";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
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
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import type { ColumnDef } from "@/components/list/DataTable";
import {
  FormPageWrapper,
  FormField,
  DetailPageWrapper,
  DetailGrid,
  DetailSkeleton,
  NotFound,
  DeleteButton,
} from "@/components/resource/shared";

const columns: ColumnDef<Project>[] = [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[140px] truncate",
    cell: (p) => p.id,
  },
  {
    key: "name",
    header: "名称",
    cell: (p) => p.name,
  },
  {
    key: "status",
    header: "状态",
    cell: (p) => (
      <Badge variant={p.status === "active" ? "default" : "secondary"}>{p.status}</Badge>
    ),
  },
  {
    key: "created",
    header: "创建时间",
    cell: (p) => new Date(p.created_at).toLocaleString(),
  },
];

export function ProjectsListPage() {
  const { role } = useAdminRole();
  const platformAdmin = isPlatformAdmin(role);

  const { data: projects = [], isLoading } = useQuery({
    queryKey: ["projects"],
    queryFn: listProjects,
  });

  const getSearchText = useCallback(
    (p: Project) => `${p.id} ${p.name} ${p.description ?? ""} ${p.status}`,
    []
  );

  return (
    <ResourceListPage
      title="Projects"
      description="管理 Torchwood 项目"
      searchPlaceholder="搜索项目名称或 ID..."
      isLoading={isLoading}
      items={projects}
      columns={columns}
      getSearchText={getSearchText}
      detailPath={(p) => `/console/projects/${p.id}`}
      toolbarActions={
        platformAdmin ? (
          <Button asChild>
            <Link to="/console/projects/new">
              <Plus className="h-4 w-4 mr-2" />
              新建项目
            </Link>
          </Button>
        ) : undefined
      }
      emptyTitle="暂无项目"
      emptyDescription="创建第一个项目以开始使用"
      emptyAction={
        platformAdmin ? (
          <Button asChild>
            <Link to="/console/projects/new">新建项目</Link>
          </Button>
        ) : undefined
      }
    />
  );
}

export function ProjectNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { role } = useAdminRole();
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");

  const mutation = useMutation({
    mutationFn: createProject,
    onSuccess: (project) => {
      toast.success("项目创建成功");
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate(`/console/projects/${project.id}`);
    },
  });

  return (
    <FormPageWrapper
      title="新建项目"
      description="添加新项目到平台"
      backTo="/console/projects"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        mutation.mutate({ id, name, description: description || undefined });
      }}
      loading={mutation.isPending}
      submitDisabled={!isPlatformAdmin(role)}
    >
      <FormField
        id="id"
        label="ID"
        value={id}
        onChange={setId}
        required
        placeholder="shop"
        hint="小写字母开头，仅小写字母与数字，最长 28。创建后不可改。"
      />
      <FormField id="name" label="名称" value={name} onChange={setName} required placeholder="My Project" />
      <FormField
        id="description"
        label="描述"
        value={description}
        onChange={setDescription}
        placeholder="可选描述"
      />
    </FormPageWrapper>
  );
}

export function ProjectDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { role } = useAdminRole();
  const { data: project, isLoading } = useQuery({
    queryKey: ["projects", id],
    queryFn: () => getProject(id!),
    enabled: !!id,
  });
  const remove = useMutation({
    mutationFn: () => deleteProject(id!),
    onSuccess: () => {
      toast.success("项目已删除");
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate("/console/projects");
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!project) return <NotFound backTo="/console/projects" />;

  const editable = isPlatformAdmin(role);

  return (
    <DetailPageWrapper
      title={project.name}
      description="项目详情"
      backTo="/console/projects"
      actions={
        editable ? (
          <DeleteButton
            label="删除项目"
            description="将永久删除该项目及其全部数据（数据库、存储元数据、函数、账本等），此操作不可撤销。"
            onConfirm={() => remove.mutate()}
            loading={remove.isPending}
          />
        ) : undefined
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: project.id, mono: true },
          { label: "名称", value: project.name },
          { label: "描述", value: project.description || "—" },
          { label: "状态", value: project.status },
          { label: "创建时间", value: new Date(project.created_at).toLocaleString() },
          { label: "更新时间", value: new Date(project.updated_at).toLocaleString() },
        ]}
      />
      <RegistrationPolicySection project={project} editable={editable} />
      <InviteCodesSection projectId={project.id} editable={editable} />
    </DetailPageWrapper>
  );
}

const REGISTRATION_POLICIES = [
  { value: "open", label: "开放注册（open）", hint: "任何人对本项目网关自助注册（默认，现状）" },
  { value: "invite_only", label: "邀请制（invite_only）", hint: "仅凭有效邀请码注册" },
  { value: "closed", label: "关闭注册（closed）", hint: "一律拒绝（403 registration_closed）" },
];

// RegistrationPolicySection 注册策略切换（T-03）：owner/admin 即改即存。
function RegistrationPolicySection({
  project,
  editable,
}: {
  project: Project;
  editable: boolean;
}) {
  const queryClient = useQueryClient();
  const policy = project.registration_policy || "open";

  const save = useMutation({
    mutationFn: (next: string) =>
      updateProject(project.id, { registration_policy: next }),
    onSuccess: () => {
      toast.success("注册策略已更新");
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
  });

  return (
    <Card className="mt-6 p-5">
      <h3 className="text-sm font-semibold">注册策略</h3>
      <p className="mt-1 text-xs text-muted-foreground">
        控制本项目网关 account.signUp 的准入语义（T-03）。
      </p>
      <div className="mt-4 flex flex-col gap-3 sm:flex-row sm:items-center">
        <Select
          value={policy}
          disabled={!editable || save.isPending}
          onValueChange={(v) => {
            if (v !== policy) save.mutate(v);
          }}
        >
          <SelectTrigger className="w-full sm:w-72">
            <SelectValue placeholder="选择注册策略" />
          </SelectTrigger>
          <SelectContent>
            {REGISTRATION_POLICIES.map((p) => (
              <SelectItem key={p.value} value={p.value}>
                {p.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">
          {REGISTRATION_POLICIES.find((p) => p.value === policy)?.hint}
        </p>
      </div>
    </Card>
  );
}

// InviteCodesSection 邀请码管理（T-03）：生成（可设次数/过期）、列表回显、
// 复制、吊销。仅 invite_only 策略下被 SignUp 消费。
function InviteCodesSection({
  projectId,
  editable,
}: {
  projectId: string;
  editable: boolean;
}) {
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);

  const { data: codes = [], isLoading } = useQuery({
    queryKey: ["invite-codes", projectId],
    queryFn: () => listInviteCodes(projectId),
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteInviteCode(projectId, id),
    onSuccess: () => {
      toast.success("邀请码已吊销");
      queryClient.invalidateQueries({ queryKey: ["invite-codes", projectId] });
    },
  });

  return (
    <Card className="mt-4 p-5">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-sm font-semibold">邀请码</h3>
          <p className="mt-1 text-xs text-muted-foreground">
            邀请制注册的准入凭证；一次性/限次/可过期/可吊销。
          </p>
        </div>
        {editable && (
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4 mr-1" />
            生成邀请码
          </Button>
        )}
      </div>
      <div className="mt-4 space-y-2">
        {isLoading ? (
          <p className="text-xs text-muted-foreground">加载中…</p>
        ) : codes.length === 0 ? (
          <p className="text-xs text-muted-foreground">暂无邀请码。</p>
        ) : (
          codes.map((c) => <InviteCodeRow key={c.id} code={c} onRevoke={() => remove.mutate(c.id)} canRevoke={editable} />)
        )}
      </div>
      <InviteCodeCreateDialog projectId={projectId} open={createOpen} onOpenChange={setCreateOpen} />
    </Card>
  );
}

function InviteCodeRow({
  code,
  canRevoke,
  onRevoke,
}: {
  code: InviteCode;
  canRevoke: boolean;
  onRevoke: () => void;
}) {
  const exhausted = code.used_count >= code.max_uses;
  const expired = code.expire_at ? new Date(code.expire_at).getTime() < Date.now() : false;
  const dead = code.revoked || exhausted || expired;
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 rounded-md border px-3 py-2">
      <code className="font-mono text-xs break-all">{code.code}</code>
      <Badge variant={dead ? "secondary" : "default"}>
        {code.revoked ? "已吊销" : exhausted ? "已用尽" : expired ? "已过期" : "有效"}
      </Badge>
      <span className="text-xs text-muted-foreground">
        {code.used_count}/{code.max_uses} 次
      </span>
      <span className="text-xs text-muted-foreground">
        {code.expire_at ? `过期 ${new Date(code.expire_at).toLocaleString()}` : "永不过期"}
      </span>
      <div className="ml-auto flex gap-2">
        <Button
          size="sm"
          variant="outline"
          onClick={() => {
            navigator.clipboard.writeText(code.code);
            toast.success("邀请码已复制");
          }}
        >
          <Copy className="h-3.5 w-3.5 mr-1" />
          复制
        </Button>
        {canRevoke && !code.revoked && (
          <Button size="sm" variant="outline" onClick={onRevoke}>
            <Trash2 className="h-3.5 w-3.5 mr-1" />
            吊销
          </Button>
        )}
      </div>
    </div>
  );
}

function InviteCodeCreateDialog({
  projectId,
  open,
  onOpenChange,
}: {
  projectId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [maxUses, setMaxUses] = useState("1");
  const [unlimited, setUnlimited] = useState(false);
  const [expireAt, setExpireAt] = useState("");
  const [created, setCreated] = useState<InviteCode | null>(null);

  const create = useMutation({
    mutationFn: () => {
      const input: { max_uses?: number; expire_at?: string } = {};
      if (!unlimited) input.max_uses = Math.max(1, parseInt(maxUses, 10) || 1);
      if (expireAt) input.expire_at = new Date(expireAt).toISOString();
      return createInviteCode(projectId, input);
    },
    onSuccess: (code) => {
      setCreated(code);
      queryClient.invalidateQueries({ queryKey: ["invite-codes", projectId] });
    },
  });

  const close = (o: boolean) => {
    if (!o) {
      setCreated(null);
      setMaxUses("1");
      setUnlimited(false);
      setExpireAt("");
    }
    onOpenChange(o);
  };

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>生成邀请码</DialogTitle>
          <DialogDescription>邀请码明文仅创建时完整显示一次。</DialogDescription>
        </DialogHeader>
        {created ? (
          <div className="space-y-4">
            <div className="rounded-md bg-muted p-3 flex items-center justify-between gap-3">
              <code className="break-all text-xs flex-1">{created.code}</code>
              <Button
                size="sm"
                variant="secondary"
                onClick={() => {
                  navigator.clipboard.writeText(created.code);
                  toast.success("邀请码已复制");
                }}
              >
                <Copy className="h-3.5 w-3.5 mr-1" />
                复制
              </Button>
            </div>
            <div className="flex justify-end">
              <Button onClick={() => close(false)}>完成</Button>
            </div>
          </div>
        ) : (
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              create.mutate();
            }}
          >
            <div className="flex items-center gap-2">
              <Checkbox
                id="invite-unlimited"
                checked={unlimited}
                onChange={(e) => setUnlimited(e.target.checked)}
              />
              <Label htmlFor="invite-unlimited" className="font-normal">
                不限次数
              </Label>
            </div>
            {!unlimited && (
              <div className="space-y-1.5">
                <Label htmlFor="invite-max-uses">最大使用次数</Label>
                <Input
                  id="invite-max-uses"
                  type="number"
                  min={1}
                  max={10000}
                  value={maxUses}
                  onChange={(e) => setMaxUses(e.target.value)}
                />
              </div>
            )}
            <div className="space-y-1.5">
              <Label htmlFor="invite-expire">过期时间（留空 = 永不过期）</Label>
              <Input
                id="invite-expire"
                type="datetime-local"
                value={expireAt}
                onChange={(e) => setExpireAt(e.target.value)}
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => close(false)}>
                取消
              </Button>
              <Button type="submit" disabled={create.isPending}>
                {create.isPending ? "生成中…" : "生成"}
              </Button>
            </div>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
