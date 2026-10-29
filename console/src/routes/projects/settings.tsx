import {
  createInviteCodeCommand,
  removeInviteCodeCommand,
  removeProjectCommand,
  updateOAuthAllowlistCommand,
  updateProjectCommand,
} from "@/kit/projects";
import {
  removeOAuthProviderCommand,
  upsertOAuthProviderCommand,
} from "@/kit/oauthProviders";
import { describeApiError } from "@/kit/errors";
import { useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { useCommand } from "@lynx-go/uikit-core";
import { toast } from "sonner";
import { Copy, Plus, Trash2 } from "lucide-react";
import {
  getProject,
  listInviteCodes,
  type Project,
  type InviteCode,
} from "@/api/projects";
import {
  OAUTH_PROVIDER_OPTIONS,
  listOAuthProviders,
  oauthCallbackURL,
  type OAuthProvider,
} from "@/api/oauthProviders";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useProjectScopeSync } from "@/hooks/useProjectScopeSync";
import { useServerPaging } from "@/hooks/useServerPaging";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { PageHeader } from "@/components/PageHeader";
import { ListPaginationKeyset } from "@/components/list/ListToolbar";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { DateTimePicker } from "@/components/ui/datetime-picker";
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
  const { id } = useParams({ strict: false });
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
        description={`Settings and management for "${project.name}"`}
      />

      <div className="flex gap-2 border-b pb-2">
        <Button
          variant={tab === "general" ? "default" : "ghost"}
          size="sm"
          onClick={() => setTab("general")}
        >
          General
        </Button>
        <Button
          variant={tab === "auth" ? "default" : "ghost"}
          size="sm"
          onClick={() => setTab("auth")}
        >
          Sign-up & Sign-in
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
          Danger Zone
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
  const tz = useUserTimezone();
  const [name, setName] = useState(project.name);
  const [description, setDescription] = useState(project.description ?? "");

  // 写路径命令化（§12.9 阶段 2）：结果两分；存量项目键经 descriptor
  // 自带 legacyKeys 宽前缀失效（§12.10-②，页面手动桥退役）。
  const update = useCommand(updateProjectCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Project info saved");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    update.mutate({ id: project.id, name, description });
  };

  return (
    <div className="space-y-6">
      <DetailGrid
        items={[
          { label: "ID", value: project.id, mono: true },
          { label: "Name", value: project.name },
          { label: "Description", value: project.description || "—" },
          { label: "Status", value: project.status },
          { label: "Created", value: formatDateTime(project.created_at, tz) },
          { label: "Updated", value: formatDateTime(project.updated_at, tz) },
        ]}
      />
      <form className="space-y-4 rounded-lg border p-6" onSubmit={onSubmit}>
        <h3 className="text-sm font-medium">Edit Basic Info</h3>
        <FormField
          id="project_name"
          label="Name"
          value={name}
          onChange={setName}
          required
          placeholder="Project name"
        />
        <FormField
          id="project_description"
          label="Description"
          value={description}
          onChange={setDescription}
          placeholder="Optional description"
        />
        <Button type="submit" disabled={!editable || update.pending}>
          {update.pending ? "Saving…" : "Save"}
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

  const remove = useCommand(removeProjectCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Project deleted");
        navigate({ to: "/console/projects" });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Card className="border-destructive/40 p-5">
      <h3 className="text-sm font-semibold text-destructive">Danger Zone</h3>
      <p className="mt-1 text-xs text-muted-foreground">
        Deleting a project permanently removes the project and all of its data
        (databases, storage metadata, functions, ledgers, etc.). This action
        cannot be undone. Only platform admins (owner/admin) can perform it.
      </p>
      <div className="mt-4">
        {editable && (
          <DeleteButton
            label="Delete Project"
            description="This will permanently delete the project and all of its data (databases, storage metadata, functions, ledgers, etc.). This action cannot be undone."
            onConfirm={() => remove.mutate({ id: project.id })}
            loading={remove.pending}
          />
        )}
      </div>
    </Card>
  );
}

const REGISTRATION_POLICIES = [
  { value: "open", label: "Open sign-up (open)", hint: "Anyone can self-register via this project's gateway (default, current behavior)" },
  { value: "invite_only", label: "Invite-only (invite_only)", hint: "Registration requires a valid invite code" },
  { value: "closed", label: "Closed (closed)", hint: "Always reject (403 registration_closed)" },
];

// RegistrationPolicySection 注册策略切换（T-03）：owner/admin 即改即存。
function RegistrationPolicySection({
  project,
  editable,
}: {
  project: Project;
  editable: boolean;
}) {
  const policy = project.registration_policy || "open";

  const save = useCommand(updateProjectCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Registration policy updated");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Card className="p-5">
      <h3 className="text-sm font-semibold">Registration Policy</h3>
      <p className="mt-1 text-xs text-muted-foreground">
        Controls who can sign up through this project's gateway via account.signUp (T-03).
      </p>
      <div className="mt-4 flex flex-col gap-3 sm:flex-row sm:items-center">
        <Select
          value={policy}
          disabled={!editable || save.pending}
          onValueChange={(v) => {
            if (v !== policy) save.mutate({ id: project.id, registration_policy: v });
          }}
        >
          <SelectTrigger className="w-full sm:w-72">
            <SelectValue placeholder="Select a registration policy" />
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
  const [rows, setRows] = useState<string[]>(
    () => project.oauth_allowed_redirect_urls ?? []
  );
  const [dirty, setDirty] = useState(false);

  const save = useCommand(updateOAuthAllowlistCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        const next = result.payload.oauth_allowed_redirect_urls ?? [];
        toast.success(
          next.length ? "Redirect allowlist updated" : "Redirect allowlist cleared, falling back to the default allowlist"
        );
        setRows(next);
        setDirty(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
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
        toast.error(`Invalid allowlist entry: ${u} (must be an absolute http/https URL)`);
        return;
      }
    }
    save.mutate({ id: project.id, urls });
  };

  return (
    <Card className="p-5">
      <h3 className="text-sm font-semibold">Redirect Allowlist</h3>
      <p className="mt-1 text-xs text-muted-foreground">
        Redirect allowlist: allowed targets for OAuth2 sign-in, magic link, recovery,
        and verification redirect flows; falls back to the default allowlist (localhost
        ranges + this site's origin) when unset. Entries match by protocol + host,
        optionally with a path prefix.
      </p>
      {!editable ? (
        <div className="mt-4 space-y-1.5">
          {(project.oauth_allowed_redirect_urls ?? []).length === 0 ? (
            <p className="text-xs text-muted-foreground">Not configured (using the default allowlist).</p>
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
                  disabled={save.pending}
                />
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    setRows(rows.filter((_, j) => j !== i));
                    setDirty(true);
                  }}
                  disabled={save.pending}
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </Button>
              </div>
            ))}
            {rows.length === 0 && (
              <p className="text-xs text-muted-foreground">
                No entries yet: saving an empty list falls back to the default allowlist.
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
              disabled={save.pending || rows.length >= 100}
            >
              <Plus className="h-3.5 w-3.5 mr-1" />
              Add Entry
            </Button>
            <Button size="sm" onClick={onSave} disabled={save.pending || !dirty}>
              {save.pending ? "Saving…" : "Save"}
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
  const [createOpen, setCreateOpen] = useState(false);
  const paging = useServerPaging();

  const { data, isLoading } = useQuery({
    queryKey: ["invite-codes", projectId, paging.pageSize, paging.pageToken],
    queryFn: () =>
      listInviteCodes(projectId, { pageSize: paging.pageSize, pageToken: paging.pageToken }),
    placeholderData: (prev) => prev,
  });
  const codes = data?.rows ?? [];

  const remove = useCommand(removeInviteCodeCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Invite code revoked");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Card className="p-5">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-sm font-semibold">Invite Codes</h3>
          <p className="mt-1 text-xs text-muted-foreground">
            Credentials for invite-only sign-up; single-use / limited-use / expirable / revocable.
          </p>
        </div>
        {editable && (
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4 mr-1" />
            Generate Invite Code
          </Button>
        )}
      </div>
      <div className="mt-4 space-y-2">
        {isLoading ? (
          <p className="text-xs text-muted-foreground">Loading…</p>
        ) : codes.length === 0 ? (
          <p className="text-xs text-muted-foreground">No invite codes yet.</p>
        ) : (
          codes.map((c) => <InviteCodeRow key={c.id} code={c} onRevoke={() => remove.mutate({ projectId, id: c.id })} canRevoke={editable} />)
        )}
      </div>
      {!isLoading && (codes.length > 0 || paging.hasPrev) && (
        <ListPaginationKeyset
          page={paging.page}
          pageSize={paging.pageSize}
          rowCount={codes.length}
          hasPrev={paging.hasPrev}
          hasNext={!!data?.nextPageToken}
          onPrev={paging.goPrev}
          onNext={() => paging.goNext(data?.nextPageToken)}
          onPageSizeChange={paging.setPageSize}
        />
      )}
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
  const tz = useUserTimezone();
  const exhausted = code.used_count >= code.max_uses;
  const expired = code.expire_at ? new Date(code.expire_at).getTime() < loadedAt : false;
  const dead = code.revoked || exhausted || expired;
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 rounded-md border px-3 py-2">
      <code className="font-mono text-xs break-all">{code.code}</code>
      <Badge variant={dead ? "secondary" : "default"}>
        {code.revoked ? "Revoked" : exhausted ? "Exhausted" : expired ? "Expired" : "Active"}
      </Badge>
      <span className="text-xs text-muted-foreground">
        {code.used_count}/{code.max_uses} uses
      </span>
      <span className="text-xs text-muted-foreground">
        {code.expire_at ? `Expires ${formatDateTime(code.expire_at, tz)}` : "Never expires"}
      </span>
      <div className="ml-auto flex gap-2">
        <Button
          size="sm"
          variant="outline"
          onClick={() => {
            navigator.clipboard.writeText(code.code);
            toast.success("Invite code copied");
          }}
        >
          <Copy className="h-3.5 w-3.5 mr-1" />
          Copy
        </Button>
        {canRevoke && !code.revoked && (
          <Button size="sm" variant="outline" onClick={onRevoke}>
            <Trash2 className="h-3.5 w-3.5 mr-1" />
            Revoke
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
  const [maxUses, setMaxUses] = useState("1");
  const [unlimited, setUnlimited] = useState(false);
  const [expireAt, setExpireAt] = useState("");
  const [created, setCreated] = useState<InviteCode | null>(null);

  // 提交收敛在 run 内（maxUses/expireAt 展示形→提交形）
  const create = useCommand(createInviteCodeCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        setCreated(result.payload);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
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
          <DialogTitle>Generate Invite Code</DialogTitle>
          <DialogDescription>The invite code is shown in full only once, at creation time.</DialogDescription>
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
                  toast.success("Invite code copied");
                }}
              >
                <Copy className="h-3.5 w-3.5 mr-1" />
                Copy
              </Button>
            </div>
            <div className="flex justify-end">
              <Button onClick={() => close(false)}>Done</Button>
            </div>
          </div>
        ) : (
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              create.mutate({
        projectId,
        ...(unlimited ? {} : { maxUses: Math.max(1, parseInt(maxUses, 10) || 1) }),
        ...(expireAt ? { expireAt } : {}),
      });
            }}
          >
            <div className="flex items-center gap-2">
              <Checkbox
                id="invite-unlimited"
                checked={unlimited}
                onChange={(e) => setUnlimited(e.target.checked)}
              />
              <Label htmlFor="invite-unlimited" className="font-normal">
                Unlimited uses
              </Label>
            </div>
            {!unlimited && (
              <div className="space-y-1.5">
                <Label htmlFor="invite-max-uses">Max uses</Label>
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
              <Label htmlFor="invite-expire">Expiry (leave empty = never expires)</Label>
              <DateTimePicker
                id="invite-expire"
                value={expireAt}
                onChange={setExpireAt}
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => close(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={create.pending}>
                {create.pending ? "Generating…" : "Generate"}
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
  const [selectedProvider, setSelectedProvider] = useState<string>("google");
  const [enabled, setEnabled] = useState(true);
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [scopesText, setScopesText] = useState("openid, email, profile");
  const platformAdmin = isPlatformAdmin(role);

  const oauthPaging = useServerPaging();
  const { data: providersData, isLoading } = useQuery({
    queryKey: ["oauth-providers", projectId, oauthPaging.pageSize, oauthPaging.pageToken],
    queryFn: () =>
      listOAuthProviders({ pageSize: oauthPaging.pageSize, pageToken: oauthPaging.pageToken }),
    enabled: !!projectId,
    placeholderData: (prev) => prev,
  });
  const providers = providersData?.rows ?? [];

  const save = useCommand(upsertOAuthProviderCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("OAuth configuration saved");
        setClientSecret("");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const remove = useCommand(removeOAuthProviderCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("OAuth configuration deleted");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
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
      toast.error("Select a project first");
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
      <p className="text-sm text-muted-foreground">Select a project in the sidebar first.</p>
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
          <Label htmlFor="enabled">Enabled</Label>
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
                (leave empty to keep the current value)
              </span>
            ) : null}
          </Label>
          <Input
            id="client_secret"
            type="password"
            value={clientSecret}
            onChange={(e) => setClientSecret(e.target.value)}
            placeholder="Required for first-time setup"
          />
        </div>

        <div className="space-y-2">
          <Label htmlFor="scopes">Scopes (comma-separated, may be empty for WeChat)</Label>
          <Input
            id="scopes"
            value={scopesText}
            onChange={(e) => setScopesText(e.target.value)}
          />
        </div>

        <p className="text-xs text-muted-foreground">
          Callback URL:
          <code className="ml-1 rounded bg-muted px-1 py-0.5">
            {oauthCallbackURL(selectedProvider)}
          </code>
        </p>

        <Button type="submit" disabled={!platformAdmin || save.pending}>
          {save.pending ? "Saving…" : "Save Configuration"}
        </Button>
      </form>

      <div className="space-y-3">
        <h3 className="text-sm font-medium">Configured</h3>
        {isLoading ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : providers.length === 0 ? (
          <p className="text-sm text-muted-foreground">No OAuth configuration yet</p>
        ) : (
          providers.map((p) => (
            <ProviderCard
              key={p.provider}
              provider={p}
              readonly={!platformAdmin}
              onEdit={() => loadProvider(p.provider)}
              onDelete={() => remove.mutate({ provider: p.provider })}
              deleting={remove.pending}
            />
          ))
        )}
        {!isLoading && (providers.length > 0 || oauthPaging.hasPrev) && (
          <ListPaginationKeyset
            page={oauthPaging.page}
            pageSize={oauthPaging.pageSize}
            rowCount={providers.length}
            hasPrev={oauthPaging.hasPrev}
            hasNext={!!providersData?.nextPageToken}
            onPrev={oauthPaging.goPrev}
            onNext={() => oauthPaging.goNext(providersData?.nextPageToken)}
            onPageSizeChange={oauthPaging.setPageSize}
          />
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
          Edit
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
        Email OTP and SMS OTP currently use<strong className="text-foreground"> platform-level </strong>
        configuration (environment variables / <code className="rounded bg-muted px-1">configs/config.yaml</code>
        ); project-level SMTP will be supported in a future release.
      </p>
      <div className="space-y-2">
        <h3 className="font-medium">Email OTP (SMTP)</h3>
        <ul className="list-disc pl-5 text-muted-foreground space-y-1">
          <li>
            <code>TORCHWOOD_MESSAGING_SMTP_HOST</code> / <code>PORT</code> /{" "}
            <code>USERNAME</code> / <code>PASSWORD</code>
          </li>
          <li>
            Dev mode: <code>TORCHWOOD_MESSAGING_DEV_LOG_OTP=true</code> writes the OTP to the server log
          </li>
        </ul>
      </div>
      <div className="space-y-2">
        <h3 className="font-medium">SMS OTP (Twilio)</h3>
        <ul className="list-disc pl-5 text-muted-foreground space-y-1">
          <li>
            <code>TORCHWOOD_MESSAGING_SMS_TWILIO_ACCOUNT_SID</code> /{" "}
            <code>AUTH_TOKEN</code> / <code>FROM</code>
          </li>
          <li>
            Dev mode: <code>TORCHWOOD_MESSAGING_DEV_LOG_SMS=true</code>
          </li>
        </ul>
      </div>
    </Card>
  );
}
