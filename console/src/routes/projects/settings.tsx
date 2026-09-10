import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Copy, Plus, Trash2 } from "lucide-react";
import {
  deleteProject,
  getProject,
  updateProject,
  updateOAuthRedirectAllowlist,
  listInviteCodes,
  createInviteCode,
  deleteInviteCode,
  type Project,
  type InviteCode,
} from "@/api/projects";
import {
  OAUTH_PROVIDER_OPTIONS,
  deleteOAuthProvider,
  listOAuthProviders,
  oauthCallbackURL,
  upsertOAuthProvider,
  type OAuthProvider,
} from "@/api/oauthProviders";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useProjectScopeSync } from "@/hooks/useProjectScopeSync";
import { PageHeader } from "@/components/PageHeader";
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
import {
  DetailGrid,
  DetailSkeleton,
  FormField,
  DeleteButton,
  NotFound,
} from "@/components/resource/shared";

type SettingsTab = "general" | "auth" | "oauth" | "danger";

// ProjectSettingsPage 项目设置区（显式作用域 /console/projects/:id/settings）：
// 项目级配置统一收敛于此（原全局 Settings 页 + 项目详情页配置分节的合并归属）。
// OAuth Providers 的 RPC 作用域来自 X-Torchwood-Project header 而非 URL，
// 因此页面挂载时经 useProjectScopeSync 把 selector 同步到 :id，并在同步完成前
// 不渲染 OAuth 面板——否则直接访问 URL 时会以旧项目身份查询/保存。
export function ProjectSettingsPage() {
  const { id } = useParams<{ id: string }>();
  const [tab, setTab] = useState<SettingsTab>("general");
  const { role } = useAdminRole();

  const { data: project, isLoading } = useQuery({
    queryKey: ["projects", id],
    queryFn: () => getProject(id!),
    enabled: !!id,
  });

  const synced = useProjectScopeSync(project?.id);

  if (isLoading) return <DetailSkeleton />;
  if (!project || !id) return <NotFound backTo="/console/projects" />;

  const editable = isPlatformAdmin(role);

  return (
    <div className="space-y-6 max-w-4xl">
      <PageHeader
        title="Settings"
        description={`项目「${project.name}」的配置与管理`}
      />

      <div className="flex gap-2 border-b pb-2">
        <Button
          variant={tab === "general" ? "default" : "ghost"}
          size="sm"
          onClick={() => setTab("general")}
        >
          基本信息
        </Button>
        <Button
          variant={tab === "auth" ? "default" : "ghost"}
          size="sm"
          onClick={() => setTab("auth")}
        >
          注册与登录
        </Button>
        <Button
          variant={tab === "oauth" ? "default" : "ghost"}
          size="sm"
          onClick={() => setTab("oauth")}
        >
          OAuth
        </Button>
        <Button
          variant={tab === "danger" ? "default" : "ghost"}
          size="sm"
          onClick={() => setTab("danger")}
        >
          危险区
        </Button>
      </div>

      {tab === "general" ? (
        <GeneralPanel project={project} editable={editable} />
      ) : tab === "auth" ? (
        <AuthPanel project={project} editable={editable} />
      ) : tab === "oauth" ? (
        synced ? (
          <OAuthPanel project={project} editable={editable} />
        ) : (
          <DetailSkeleton />
        )
      ) : (
        <DangerZonePanel project={project} editable={editable} />
      )}
    </div>
  );
}

function GeneralPanel({
  project,
  editable,
}: {
  project: Project;
  editable: boolean;
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState(project.name);
  const [description, setDescription] = useState(project.description ?? "");

  const update = useMutation({
    mutationFn: () =>
      updateProject(project.id, { name, description }),
    onSuccess: () => {
      toast.success("项目信息已保存");
      queryClient.invalidateQueries({ queryKey: ["projects", project.id] });
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
  });

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    update.mutate();
  };

  return (
    <div className="space-y-6">
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
      <form className="space-y-4 rounded-lg border p-6" onSubmit={onSubmit}>
        <h3 className="text-sm font-medium">编辑基本信息</h3>
        <FormField
          id="project_name"
          label="名称"
          value={name}
          onChange={setName}
          required
          placeholder="项目名称"
        />
        <FormField
          id="project_description"
          label="描述"
          value={description}
          onChange={setDescription}
          placeholder="可选描述"
        />
        <Button type="submit" disabled={!editable || update.isPending}>
          {update.isPending ? "保存中…" : "保存"}
        </Button>
      </form>
    </div>
  );
}

function AuthPanel({
  project,
  editable,
}: {
  project: Project;
  editable: boolean;
}) {
  return (
    <div className="space-y-4">
      <RegistrationPolicySection project={project} editable={editable} />
      <InviteCodesSection projectId={project.id} editable={editable} />
      <MessagingPanel />
    </div>
  );
}

function OAuthPanel({
  project,
  editable,
}: {
  project: Project;
  editable: boolean;
}) {
  return (
    <div className="space-y-4">
      <OAuthProvidersPanel />
      <RedirectAllowlistSection project={project} editable={editable} />
    </div>
  );
}

function DangerZonePanel({
  project,
  editable,
}: {
  project: Project;
  editable: boolean;
}) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const remove = useMutation({
    mutationFn: () => deleteProject(project.id),
    onSuccess: () => {
      toast.success("项目已删除");
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate("/console/projects");
    },
  });

  return (
    <Card className="border-destructive/40 p-5">
      <h3 className="text-sm font-semibold text-destructive">Danger Zone</h3>
      <p className="mt-1 text-xs text-muted-foreground">
        删除项目将永久移除该项目及其全部数据（数据库、存储元数据、函数、账本等），
        此操作不可撤销。仅平台管理员（owner/admin）可执行。
      </p>
      <div className="mt-4">
        {editable && (
          <DeleteButton
            label="删除项目"
            description="将永久删除该项目及其全部数据（数据库、存储元数据、函数、账本等），此操作不可撤销。"
            onConfirm={() => remove.mutate()}
            loading={remove.isPending}
          />
        )}
      </div>
    </Card>
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
    <Card className="p-5">
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

// RedirectAllowlistSection 重定向白名单（settings auth.oauth_allowed_redirect_urls）：
// 约束 OAuth2 登录 / 魔法链接 / 恢复 / 验证等重定向流的可落点（钓鱼劫持面）。
// 整表替换语义：空列表 = 未配置，回落默认白名单（localhost 系列 + 本站 origin）。
// 条目匹配协议+主机（可含路径前缀）。
function RedirectAllowlistSection({
  project,
  editable,
}: {
  project: Project;
  editable: boolean;
}) {
  const queryClient = useQueryClient();
  const [rows, setRows] = useState<string[]>(
    () => project.oauth_allowed_redirect_urls ?? []
  );
  const [dirty, setDirty] = useState(false);

  const save = useMutation({
    mutationFn: (urls: string[]) =>
      updateOAuthRedirectAllowlist(project.id, urls),
    onSuccess: (p) => {
      const next = p.oauth_allowed_redirect_urls ?? [];
      toast.success(
        next.length ? "重定向白名单已更新" : "重定向白名单已清空，回落默认白名单"
      );
      setRows(next);
      setDirty(false);
      queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
  });

  const onSave = () => {
    const urls = rows.map((r) => r.trim()).filter(Boolean);
    for (const u of urls) {
      let ok: boolean;
      try {
        const parsed = new URL(u);
        ok = parsed.protocol === "http:" || parsed.protocol === "https:";
      } catch {
        ok = false;
      }
      if (!ok) {
        toast.error(`无效的白名单条目：${u}（须为 http/https 绝对地址）`);
        return;
      }
    }
    save.mutate(urls);
  };

  return (
    <Card className="p-5">
      <h3 className="text-sm font-semibold">Redirect Allowlist</h3>
      <p className="mt-1 text-xs text-muted-foreground">
        重定向白名单：OAuth2 登录、魔法链接、恢复、验证等重定向流的允许落点；
        未配置时回落默认白名单（localhost 系列 + 本站 origin）。条目按协议+主机匹配，可含路径前缀。
      </p>
      {!editable ? (
        <div className="mt-4 space-y-1.5">
          {(project.oauth_allowed_redirect_urls ?? []).length === 0 ? (
            <p className="text-xs text-muted-foreground">未配置（使用默认白名单）。</p>
          ) : (
            (project.oauth_allowed_redirect_urls ?? []).map((u) => (
              <code key={u} className="block rounded-md border px-3 py-1.5 font-mono text-xs break-all">
                {u}
              </code>
            ))
          )}
        </div>
      ) : (
        <>
          <div className="mt-4 space-y-2">
            {rows.map((row, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  value={row}
                  onChange={(e) => {
                    setRows(rows.map((r, j) => (j === i ? e.target.value : r)));
                    setDirty(true);
                  }}
                  placeholder="https://app.example.com"
                  className="font-mono text-xs"
                  disabled={save.isPending}
                />
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    setRows(rows.filter((_, j) => j !== i));
                    setDirty(true);
                  }}
                  disabled={save.isPending}
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </Button>
              </div>
            ))}
            {rows.length === 0 && (
              <p className="text-xs text-muted-foreground">
                暂无条目：保存空列表即回落默认白名单。
              </p>
            )}
          </div>
          <div className="mt-3 flex items-center gap-2">
            <Button
              size="sm"
              variant="outline"
              onClick={() => {
                setRows([...rows, ""]);
                setDirty(true);
              }}
              disabled={save.isPending || rows.length >= 100}
            >
              <Plus className="h-3.5 w-3.5 mr-1" />
              添加条目
            </Button>
            <Button size="sm" onClick={onSave} disabled={save.isPending || !dirty}>
              {save.isPending ? "保存中…" : "保存"}
            </Button>
          </div>
        </>
      )}
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
    <Card className="p-5">
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
  // 过期展示按行加载时刻判定：render 内不得调用 Date.now（react-hooks/purity），
  // useState 初始化器是 React 认可的一次性求值点。
  const [loadedAt] = useState(() => Date.now());
  const exhausted = code.used_count >= code.max_uses;
  const expired = code.expire_at ? new Date(code.expire_at).getTime() < loadedAt : false;
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

function OAuthProvidersPanel() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const queryClient = useQueryClient();
  const [selectedProvider, setSelectedProvider] = useState<string>("google");
  const [enabled, setEnabled] = useState(true);
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [scopesText, setScopesText] = useState("openid, email, profile");
  const platformAdmin = isPlatformAdmin(role);

  const { data: providers = [], isLoading } = useQuery({
    queryKey: ["oauth-providers", projectId],
    queryFn: listOAuthProviders,
    enabled: !!projectId,
  });

  const save = useMutation({
    mutationFn: upsertOAuthProvider,
    onSuccess: () => {
      toast.success("OAuth 配置已保存");
      setClientSecret("");
      queryClient.invalidateQueries({ queryKey: ["oauth-providers"] });
    },
  });

  const remove = useMutation({
    mutationFn: deleteOAuthProvider,
    onSuccess: () => {
      toast.success("OAuth 配置已删除");
      queryClient.invalidateQueries({ queryKey: ["oauth-providers"] });
    },
  });

  const loadProvider = (provider: string) => {
    setSelectedProvider(provider);
    const existing = providers.find((p) => p.provider === provider);
    const defaults = OAUTH_PROVIDER_OPTIONS.find((o) => o.id === provider);
    if (existing) {
      setEnabled(existing.enabled);
      setClientId(existing.client_id);
      setClientSecret("");
      setScopesText((existing.scopes ?? []).join(", "));
    } else {
      setEnabled(true);
      setClientId("");
      setClientSecret("");
      setScopesText((defaults?.defaultScopes ?? []).join(", "));
    }
  };

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!projectId) {
      toast.error("请先选择项目");
      return;
    }
    const scopes = scopesText
      .split(/[,\s]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    save.mutate({
      provider: selectedProvider,
      enabled,
      client_id: clientId,
      client_secret: clientSecret || undefined,
      scopes,
    });
  };

  if (!projectId) {
    return (
      <p className="text-sm text-muted-foreground">请先在侧边栏选择一个项目。</p>
    );
  }

  return (
    <div className="grid gap-8 lg:grid-cols-[1fr_320px]">
      <form className="space-y-4 rounded-lg border p-6" onSubmit={onSubmit}>
        <div className="space-y-2">
          <Label>Provider</Label>
          <Select
            value={selectedProvider}
            onValueChange={(v) => loadProvider(v)}
          >
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {OAUTH_PROVIDER_OPTIONS.map((opt) => (
                <SelectItem key={opt.id} value={opt.id}>
                  {opt.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        <div className="flex items-center gap-2">
          <Checkbox
            id="enabled"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          <Label htmlFor="enabled">启用</Label>
        </div>

        <div className="space-y-2">
          <Label htmlFor="client_id">Client ID / AppID</Label>
          <Input
            id="client_id"
            value={clientId}
            onChange={(e) => setClientId(e.target.value)}
            required
          />
        </div>

        <div className="space-y-2">
          <Label htmlFor="client_secret">
            Client Secret / AppSecret
            {providers.find((p) => p.provider === selectedProvider)?.has_client_secret ? (
              <span className="text-muted-foreground font-normal ml-2">
                （留空则保留原值）
              </span>
            ) : null}
          </Label>
          <Input
            id="client_secret"
            type="password"
            value={clientSecret}
            onChange={(e) => setClientSecret(e.target.value)}
            placeholder="首次配置必填"
          />
        </div>

        <div className="space-y-2">
          <Label htmlFor="scopes">Scopes（逗号分隔，微信可留空）</Label>
          <Input
            id="scopes"
            value={scopesText}
            onChange={(e) => setScopesText(e.target.value)}
          />
        </div>

        <p className="text-xs text-muted-foreground">
          回调地址：
          <code className="ml-1 rounded bg-muted px-1 py-0.5">
            {oauthCallbackURL(selectedProvider)}
          </code>
        </p>

        <Button type="submit" disabled={!platformAdmin || save.isPending}>
          {save.isPending ? "保存中…" : "保存配置"}
        </Button>
      </form>

      <div className="space-y-3">
        <h3 className="text-sm font-medium">已配置</h3>
        {isLoading ? (
          <p className="text-sm text-muted-foreground">加载中…</p>
        ) : providers.length === 0 ? (
          <p className="text-sm text-muted-foreground">暂无 OAuth 配置</p>
        ) : (
          providers.map((p) => (
            <ProviderCard
              key={p.provider}
              provider={p}
              readonly={!platformAdmin}
              onEdit={() => loadProvider(p.provider)}
              onDelete={() => remove.mutate(p.provider)}
              deleting={remove.isPending}
            />
          ))
        )}
      </div>
    </div>
  );
}

function ProviderCard({
  provider,
  onEdit,
  onDelete,
  deleting,
  readonly,
}: {
  provider: OAuthProvider;
  onEdit: () => void;
  onDelete: () => void;
  deleting: boolean;
  readonly: boolean;
}) {
  const label =
    OAUTH_PROVIDER_OPTIONS.find((o) => o.id === provider.provider)?.label ??
    provider.provider;

  return (
    <div className="rounded-lg border p-3 space-y-2">
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium text-sm">{label}</span>
        <Badge variant={provider.enabled ? "default" : "secondary"}>
          {provider.enabled ? "Enabled" : "Disabled"}
        </Badge>
      </div>
      <p className="text-xs text-muted-foreground font-mono truncate">
        {provider.client_id}
      </p>
      <div className="flex gap-2">
        <Button type="button" variant="outline" size="sm" onClick={onEdit} disabled={readonly}>
          编辑
        </Button>
        {!readonly && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="text-destructive"
            disabled={deleting}
            onClick={onDelete}
          >
            <Trash2 className="h-4 w-4" />
          </Button>
        )}
      </div>
    </div>
  );
}

function MessagingPanel() {
  return (
    <Card className="p-5 space-y-4 text-sm">
      <p className="text-muted-foreground">
        Email OTP 与 SMS OTP 目前使用<strong className="text-foreground">平台级</strong>
        配置（环境变量 / <code className="rounded bg-muted px-1">configs/config.yaml</code>
        ），项目级 SMTP 将在后续版本支持。
      </p>
      <div className="space-y-2">
        <h3 className="font-medium">Email OTP（SMTP）</h3>
        <ul className="list-disc pl-5 text-muted-foreground space-y-1">
          <li>
            <code>TORCHWOOD_MESSAGING_SMTP_HOST</code> / <code>PORT</code> /{" "}
            <code>USERNAME</code> / <code>PASSWORD</code>
          </li>
          <li>
            开发模式：<code>TORCHWOOD_MESSAGING_DEV_LOG_OTP=true</code> 将验证码写入服务日志
          </li>
        </ul>
      </div>
      <div className="space-y-2">
        <h3 className="font-medium">SMS OTP（Twilio）</h3>
        <ul className="list-disc pl-5 text-muted-foreground space-y-1">
          <li>
            <code>TORCHWOOD_MESSAGING_SMS_TWILIO_ACCOUNT_SID</code> /{" "}
            <code>AUTH_TOKEN</code> / <code>FROM</code>
          </li>
          <li>
            开发模式：<code>TORCHWOOD_MESSAGING_DEV_LOG_SMS=true</code>
          </li>
        </ul>
      </div>
    </Card>
  );
}
