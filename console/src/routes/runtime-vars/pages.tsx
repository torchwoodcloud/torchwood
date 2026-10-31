import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useCollection, useCollectionData, useCommand, useRecord } from "@lynx-go/uikit-core";
import type { ApiError, CommandResult } from "@lynx-go/uikit-core/kernel";
import { toast } from "sonner";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import {
  ArrowRightLeft,
  Eye,
  History,
  Pencil,
  Plus,
  ShieldAlert,
  Trash2,
} from "lucide-react";
import {
  type RuntimeVar,
  type RuntimeVarValue,
  type RuntimeVarVersion,
  type RuntimeVarVersionEntry,
  type VarSet,
  type VarSetVisibilityInput,
} from "@/api/runtimeVars";
import {
  createVarCommand,
  createVarSetCommand,
  editVarDescriptor,
  editVarSetDescriptor,
  removeVarCommand,
  removeVarSetCommand,
  rollbackVarSetCommand,
  runtimeVarVersionResource,
  runtimeVarVersionsResource,
  runtimeVarsResource,
  type VarKind,
  varSetsResource,
  varSetsSearch,
} from "@/kit/runtimeVars";
import { describeApiError } from "@/kit/errors";
import { useAuth } from "@/hooks/useAuth";
import { isPlatformAdmin, useAdminRole } from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { EmptyState } from "@/components/EmptyState";
import { LoadingTable } from "@/components/LoadingTable";
import { PageHeader } from "@/components/PageHeader";
import {
  DetailPageWrapper,
  DetailSkeleton,
  NotFound,
} from "@/components/resource/shared";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

// RuntimeVars Console 管理面（docs/design/runtime-vars.md §2.7）：集合列表页 +
// 集合详情页（变量编辑 / 版本历史 / 集合设置三区块）。写动词（Create/Update/
// Delete/Rollback）admin/owner 专属——按钮按 isPlatformAdmin 收口；读动词全角色
//（viewer 含），与 api-keys 页同模式。

// D13 限额常量（展示用；服务端是唯一执法点）。
const SET_LIMIT_BYTES = 1024 * 1024;
const VAR_COUNT_LIMIT = 500;
const ID_PATTERN = "^[a-z_][a-z0-9_]*$";
const KEY_PATTERN = "^[a-z_][a-z0-9_]*$";

const ACTION_LABEL: Record<string, string> = {
  RUNTIME_VAR_VERSION_ACTION_CREATE: "Create",
  RUNTIME_VAR_VERSION_ACTION_UPDATE: "Update",
  RUNTIME_VAR_VERSION_ACTION_DELETE: "Delete",
  RUNTIME_VAR_VERSION_ACTION_ROLLBACK: "Rollback",
};

const VAR_KINDS: VarKind[] = ["string", "integer", "float", "boolean", "json"];

function toVisibility(v: string): VarSetVisibilityInput {
  return v === "VAR_SET_VISIBILITY_PRIVATE"
    ? "VAR_SET_VISIBILITY_PRIVATE"
    : "VAR_SET_VISIBILITY_PUBLIC";
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < SET_LIMIT_BYTES) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / SET_LIMIT_BYTES).toFixed(2)} MB`;
}

function truncate(text: string, max = 80): string {
  return text.length > max ? `${text.slice(0, max)}…` : text;
}

// detectKind 从 oneof 响应推断当前类型（服务端保证恰有一个分支被设置）。
function detectKind(v: RuntimeVarValue | undefined): VarKind {
  if (!v) return "string";
  if (v.string_value !== undefined) return "string";
  if (v.integer_value !== undefined) return "integer";
  if (v.float_value !== undefined) return "float";
  if (v.bool_value !== undefined) return "boolean";
  if (v.json_value !== undefined) return "json";
  return "string";
}

// formatValueText 展示值文本：json 尝试美化，其余按字面。
function formatValueText(v: RuntimeVarValue | undefined): string {
  if (!v) return "—";
  if (v.string_value !== undefined) return v.string_value;
  if (v.integer_value !== undefined) return String(v.integer_value);
  if (v.float_value !== undefined) return String(v.float_value);
  if (v.bool_value !== undefined) return String(v.bool_value);
  if (v.json_value !== undefined) {
    try {
      return JSON.stringify(JSON.parse(v.json_value), null, 2);
    } catch {
      return v.json_value;
    }
  }
  return "—";
}

// SecretWarningBanner 两页顶部固定警示条（§2.7：public 集 = 持 project_id 的
// 客户端匿名可读；private 集 = 登录用户可见；禁止存放机密）。
function SecretWarningBanner() {
  return (
    <div className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 px-4 py-3 text-sm text-amber-700 dark:text-amber-400">
      <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
      <p>
        Variables are readable by any client holding the project_id (public sets)
        or visible to signed-in users only (private sets). Never store secrets
        (API keys, passwords, private keys, etc.).
      </p>
    </div>
  );
}

function VisibilityBadge({ visibility }: { visibility: string }) {
  const isPublic = visibility !== "VAR_SET_VISIBILITY_PRIVATE";
  return (
    <Badge variant={isPublic ? "default" : "secondary"} title={visibility}>
      {isPublic ? "Public" : "Private"}
    </Badge>
  );
}

function ActionBadge({ action }: { action: string }) {
  const label = ACTION_LABEL[action] ?? action;
  const variant =
    action === "RUNTIME_VAR_VERSION_ACTION_DELETE"
      ? "destructive"
      : action === "RUNTIME_VAR_VERSION_ACTION_ROLLBACK"
        ? "secondary"
        : "default";
  return (
    <Badge variant={variant} title={action}>
      {label}
    </Badge>
  );
}

function TypeBadge({ kind }: { kind: VarKind }) {
  return (
    <Badge variant="outline" className="font-mono text-xs">
      {kind}
    </Badge>
  );
}

function FootprintCell({ bytes }: { bytes: number }) {
  const pct = Math.min(100, (bytes / SET_LIMIT_BYTES) * 100);
  return (
    <div className="w-44 space-y-1">
      <Progress value={pct} className="h-1.5" />
      <p className="text-xs text-muted-foreground">
        {formatBytes(bytes)} / 1 MB
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// 列表页
// ---------------------------------------------------------------------------

export function VarSetsListPage() {
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const navigate = useNavigate();
  const [createOpen, setCreateOpen] = useState(false);
  const [search, setSearch] = useState("");
  const platformAdmin = isPlatformAdmin(role);

  // 列表 fleet 化（§12.9-2）：游标/页大小进 URL（分页栈 = 浏览器历史）；
  // 页内当前页搜索保持本地态（旧页语义）。vars/versions 子视图与表单不动。
  // scope/enabled 不再逐处传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用。
  const list = useCollection(varSetsResource, {
    from: "/console/runtime-vars",
    search: varSetsSearch,
  });

  const getSearchText = useCallback(
    (s: VarSet) => `${s.var_set_id} ${s.description}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((s) => getSearchText(s).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const columns: LegacyColumnDef<VarSet, unknown>[] = [
    {
      id: "var_set_id",
      header: "Set ID",
      cell: ({ row }) => (
        <span className="font-mono text-xs">{row.original.var_set_id}</span>
      ),
    },
    {
      id: "visibility",
      header: "Visibility",
      cell: ({ row }) => <VisibilityBadge visibility={row.original.visibility} />,
    },
    {
      id: "var_count",
      header: "Variables",
      cell: ({ row }) => `${row.original.var_count} / ${VAR_COUNT_LIMIT}`,
    },
    {
      id: "revision",
      header: "Current version",
      cell: ({ row }) => `#${row.original.revision}`,
    },
    {
      id: "footprint",
      header: "Usage (limit 1 MB)",
      cell: ({ row }) => <FootprintCell bytes={row.original.total_bytes} />,
    },
    {
      id: "updated_at",
      header: "Updated at",
      cell: ({ row }) => formatDateTime(row.original.updated_at, tz),
    },
  ];

  return (
    <div className="space-y-6">
      <SecretWarningBanner />
      <PageHeader title="Runtime Vars" description="Manage project-level runtime configuration: typed variable sets, visibility, and version rollback" />
      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search set ID or description on this page…"
              className="h-8 w-56"
            />
            {platformAdmin ? (
              <Button size="sm" className="ml-auto" onClick={() => setCreateOpen(true)}>
                <Plus className="h-4 w-4 mr-2" />
                New variable set
              </Button>
            ) : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching variable sets" : "No variable sets yet"}
        emptyDescription={
          search !== ""
            ? "Adjust the search term, or clear it to restore all rows on this page"
            : "Create your first variable set to ship feature flags, copy, thresholds, and other runtime configuration to clients"
        }
        emptyAction={
          platformAdmin ? (
            <Button onClick={() => setCreateOpen(true)}>New variable set</Button>
          ) : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={columns}
            getRowId={(s) => s.id}
            onRowClick={(s) => navigate({ to: `/console/runtime-vars/${s.var_set_id}` })}
          />
        </div>
      </KitListPage>
      <VarSetCreateDialog open={createOpen} onOpenChange={setCreateOpen} />
    </div>
  );
}

// VarSetCreateDialog 新建集合（var_set_id 创建后不可改，D9）。命令化：表单
// 本体不动；kit 键随资源命令 ok 自动失效（同资源缺省——onCreated refetch 桥
// 与存量前缀桥均已退役）；结果分支无表单容器 → toast。
function VarSetCreateDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [varSetId, setVarSetId] = useState("");
  const [visibility, setVisibility] =
    useState<VarSetVisibilityInput>("VAR_SET_VISIBILITY_PUBLIC");
  const [description, setDescription] = useState("");

  useEffect(() => {
    if (open) {
      setVarSetId("");
      setVisibility("VAR_SET_VISIBILITY_PUBLIC");
      setDescription("");
    }
  }, [open]);

  const create = useCommand(createVarSetCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Variable set created");
        onOpenChange(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>New variable set</DialogTitle>
          <DialogDescription>
            A set is a container for variables; it cannot be renamed after
            creation (recreate + rollback instead).
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate({ varSetId, visibility, description });
          }}
        >
          <div className="space-y-1.5">
            <Label htmlFor="var-set-id">Set ID</Label>
            <Input
              id="var-set-id"
              value={varSetId}
              onChange={(e) => setVarSetId(e.target.value)}
              required
              maxLength={40}
              pattern={ID_PATTERN}
              placeholder="feature_flags"
              className="font-mono text-xs"
            />
            <p className="text-xs text-muted-foreground">
              Lowercase letters, digits, and underscores; must start with a
              letter or underscore, ≤40 characters; immutable after creation.
            </p>
          </div>
          <div className="space-y-1.5">
            <Label>Visibility</Label>
            <Select value={visibility} onValueChange={(v) => setVisibility(v as VarSetVisibilityInput)}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="VAR_SET_VISIBILITY_PUBLIC">
                  Public — readable anonymously by any client holding the project_id
                </SelectItem>
                <SelectItem value="VAR_SET_VISIBILITY_PRIVATE">
                  Private — visible to signed-in project users only
                </SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="var-set-description">Description (optional)</Label>
            <Input
              id="var-set-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              maxLength={256}
              placeholder="Client feature flags and thresholds"
            />
          </div>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.pending}>
              {create.pending ? "Creating…" : "Create"}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// 详情页（三区块）
// ---------------------------------------------------------------------------

export function VarSetDetailPage() {
  const { varSetId = "" } = useParams({ strict: false });
  const navigate = useNavigate();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const platformAdmin = isPlatformAdmin(role);

  const [createVarOpen, setCreateVarOpen] = useState(false);
  const [editingVar, setEditingVar] = useState<RuntimeVar | null>(null);
  const [deletingVarKey, setDeletingVarKey] = useState<string | null>(null);
  const [viewRevision, setViewRevision] = useState<number | null>(null);
  const [rollbackVersion, setRollbackVersion] = useState<RuntimeVarVersion | null>(null);
  const [diffPair, setDiffPair] = useState<{ a: number; b: number } | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [deleteSetOpen, setDeleteSetOpen] = useState(false);
  const [diffA, setDiffA] = useState("");
  const [diffB, setDiffB] = useState("");

  // 子视图 kit 化批：集合详情读换轨 useRecord、vars/versions 子列表换轨
  // useCollectionData（整取方言 window: "all"）——id/scope 未就绪即自动停用
  //（与旧 enabled: !!projectId && !!varSetId 同义）；失效 = var 级命令的
  // invalidates 声明（kit 键全 kind 命中，legacy 前缀桥退役）。
  const varSetRecord = useRecord(varSetsResource, {
    id: varSetId || undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const varsData = useCollectionData(runtimeVarsResource, {
    scope: { projectId: projectId ?? undefined, varSetId: varSetId || undefined },
    window: "all",
  });
  const versionsData = useCollectionData(runtimeVarVersionsResource, {
    scope: { projectId: projectId ?? undefined, varSetId: varSetId || undefined },
    window: "all",
  });

  const vars = varsData.rows;
  // 行数组引用由 query data 单源派生（跨渲染稳定），可直接作 effect 依赖
  //（无旧 `?? []` 派生数组每帧换身份问题）。
  const versions = versionsData.rows;

  // 写路径命令化：var 级命令（varCreate/varRemove/rollback）的失效 = descriptor
  // invalidates 声明（三资源 kit 键全 kind——详情 record/vars/versions 列表/
  // 版本快照一并刷新，§12.10-② 存量前缀桥退役）。删除/回滚走 ConfirmDialog
  //（无表单容器）——结果分支 toast。
  const removeVar = useCommand(removeVarCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Variable deleted");
        setDeletingVarKey(null);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const removeSet = useCommand(removeVarSetCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Variable set deleted");
        navigate({ to: "/console/runtime-vars" });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const rollbackMut = useCommand(rollbackVarSetCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        if (rollbackVersion) {
          toast.success(
            `Rolled back to version #${rollbackVersion.revision}, created new version #${result.payload.revision}`
          );
        }
        setRollbackVersion(null);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 路由参数变化（同组件实例复用）时重置本地状态。
  useEffect(() => {
    setDiffA("");
    setDiffB("");
    setViewRevision(null);
    setDiffPair(null);
    setEditingVar(null);
    setDeletingVarKey(null);
    setRollbackVersion(null);
  }, [varSetId]);

  // 版本对比缺省选中：A = 最老版（基准），B = 最新版。
  useEffect(() => {
    if (versions.length === 0 || diffA || diffB) return;
    const revs = versions.map((v) => v.revision);
    setDiffA(String(Math.min(...revs)));
    setDiffB(String(Math.max(...revs)));
  }, [versions, diffA, diffB]);

  const diffReady =
    !!diffA && !!diffB && diffA !== diffB && versions.length >= 2;

  if (varSetRecord.status === "loading") return <DetailSkeleton />;
  const varSet = varSetRecord.data;
  if (!varSet) return <NotFound backTo="/console/runtime-vars" />;

  return (
    <div className="space-y-6">
      <SecretWarningBanner />
      <DetailPageWrapper
        title={varSet.var_set_id}
        description={varSet.description || "Runtime variable set"}
        backTo="/console/runtime-vars"
      >
        <Card>
          <CardContent className="pt-6">
            <dl className="grid gap-4 sm:grid-cols-3">
              <div>
                <dt className="text-sm text-muted-foreground">Visibility</dt>
                <dd className="mt-1">
                  <VisibilityBadge visibility={varSet.visibility} />
                </dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">Current version</dt>
                <dd className="mt-1 font-medium">#{varSet.revision}</dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">Variables</dt>
                <dd className="mt-1 font-medium">
                  {varSet.var_count} / {VAR_COUNT_LIMIT}
                </dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">Total value size</dt>
                <dd className="mt-1 font-medium">
                  {formatBytes(varSet.total_bytes)} / 1 MB
                </dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">ETag</dt>
                <dd className="mt-1 font-mono text-sm break-all">{varSet.etag}</dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">Updated at</dt>
                <dd className="mt-1 font-medium">
                  {formatDateTime(varSet.updated_at, tz)}
                </dd>
              </div>
            </dl>
          </CardContent>
        </Card>

        {/* 区块 ①：变量编辑表格 */}
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0">
            <CardTitle>Variables ({vars.length})</CardTitle>
            {platformAdmin && (
              <Button size="sm" onClick={() => setCreateVarOpen(true)}>
                <Plus className="h-4 w-4 mr-2" />
                New variable
              </Button>
            )}
          </CardHeader>
          <CardContent>
            {varsData.status === "loading" ? (
              <LoadingTable columns={5} />
            ) : vars.length === 0 ? (
              <EmptyState
                title="No variables yet"
                description="Create typed variables; clients fetch the whole set with ETag polling"
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Key</TableHead>
                    <TableHead>Type</TableHead>
                    <TableHead>Value</TableHead>
                    <TableHead>Updated at</TableHead>
                    {platformAdmin && <TableHead className="text-right">Actions</TableHead>}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {vars.map((v) => {
                    const kind = detectKind(v.value);
                    const preview = formatValueText(v.value);
                    return (
                      <TableRow key={v.key}>
                        <TableCell className="font-mono text-xs">{v.key}</TableCell>
                        <TableCell>
                          <TypeBadge kind={kind} />
                        </TableCell>
                        <TableCell
                          className="max-w-[320px] truncate font-mono text-xs"
                          title={preview}
                        >
                          {truncate(preview)}
                        </TableCell>
                        <TableCell>{formatDateTime(v.updated_at, tz)}</TableCell>
                        {platformAdmin && (
                          <TableCell className="text-right">
                            <div className="flex items-center justify-end gap-1">
                              <Button
                                variant="ghost"
                                size="icon"
                                onClick={() => setEditingVar(v)}
                                title="Edit"
                              >
                                <Pencil className="h-4 w-4" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                onClick={() => setDeletingVarKey(v.key)}
                                title="Delete"
                              >
                                <Trash2 className="h-4 w-4 text-destructive" />
                              </Button>
                            </div>
                          </TableCell>
                        )}
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>

        {/* 区块 ②：版本历史（保留最近 50 版，D11） */}
        <Card>
          <CardHeader className="space-y-4">
            <CardTitle>Version history ({versions.length}, last 50 kept)</CardTitle>
            <div className="flex flex-wrap items-end gap-2">
              <div className="space-y-1">
                <Label className="text-xs text-muted-foreground">Old version (base)</Label>
                <Select value={diffA} onValueChange={setDiffA}>
                  <SelectTrigger className="w-36">
                    <SelectValue placeholder="Select version" />
                  </SelectTrigger>
                  <SelectContent>
                    {versions.map((v) => (
                      <SelectItem key={v.revision} value={String(v.revision)}>
                        #{v.revision}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1">
                <Label className="text-xs text-muted-foreground">New version (compare)</Label>
                <Select value={diffB} onValueChange={setDiffB}>
                  <SelectTrigger className="w-36">
                    <SelectValue placeholder="Select version" />
                  </SelectTrigger>
                  <SelectContent>
                    {versions.map((v) => (
                      <SelectItem key={v.revision} value={String(v.revision)}>
                        #{v.revision}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <Button
                variant="outline"
                disabled={!diffReady}
                onClick={() => setDiffPair({ a: Number(diffA), b: Number(diffB) })}
              >
                <ArrowRightLeft className="h-4 w-4 mr-2" />
                Compare versions
              </Button>
            </div>
          </CardHeader>
          <CardContent>
            {versionsData.status === "loading" ? (
              <LoadingTable columns={6} />
            ) : versions.length === 0 ? (
              <EmptyState
                title="No versions yet"
                description="Variable creates, updates, deletes, and rollbacks automatically produce version records"
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Version</TableHead>
                    <TableHead>Action</TableHead>
                    <TableHead>Summary</TableHead>
                    <TableHead>Actor</TableHead>
                    <TableHead>Time</TableHead>
                    {platformAdmin && <TableHead className="text-right">Actions</TableHead>}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {versions.map((v) => {
                    const isCurrent = v.revision === varSet.revision;
                    return (
                      <TableRow key={v.revision}>
                        <TableCell className="font-mono text-xs">#{v.revision}</TableCell>
                        <TableCell>
                          <ActionBadge action={v.action} />
                        </TableCell>
                        <TableCell
                          className="max-w-[240px] truncate text-xs"
                          title={v.summary}
                        >
                          {v.summary || "—"}
                        </TableCell>
                        <TableCell className="font-mono text-xs">{v.actor}</TableCell>
                        <TableCell>{formatDateTime(v.created_at, tz)}</TableCell>
                        {platformAdmin && (
                          <TableCell className="text-right">
                            <div className="flex items-center justify-end gap-1">
                              <Button
                                variant="ghost"
                                size="icon"
                                onClick={() => setViewRevision(v.revision)}
                                title="View snapshot"
                              >
                                <Eye className="h-4 w-4" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                disabled={isCurrent}
                                onClick={() => setRollbackVersion(v)}
                                title={isCurrent ? "Already the current version" : "Roll back to this version"}
                              >
                                <History className="h-4 w-4" />
                              </Button>
                            </div>
                          </TableCell>
                        )}
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>

        {/* 区块 ③：集合设置 */}
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0">
            <CardTitle>Set settings</CardTitle>
            {platformAdmin && (
              <div className="flex gap-2">
                <Button variant="outline" size="sm" onClick={() => setSettingsOpen(true)}>
                  <Pencil className="h-4 w-4 mr-2" />
                  Edit settings
                </Button>
                <Button variant="destructive" size="sm" onClick={() => setDeleteSetOpen(true)}>
                  <Trash2 className="h-4 w-4 mr-2" />
                  Delete set
                </Button>
              </div>
            )}
          </CardHeader>
          <CardContent>
            <dl className="grid gap-4 sm:grid-cols-2">
              <div>
                <dt className="text-sm text-muted-foreground">Visibility</dt>
                <dd className="mt-1">
                  <VisibilityBadge visibility={varSet.visibility} />
                  <p className="mt-1 text-xs text-muted-foreground">
                    {varSet.visibility === "VAR_SET_VISIBILITY_PRIVATE"
                      ? "Private: visible to signed-in project users only; anonymous clients get a 404."
                      : "Public: readable anonymously by any client holding the project_id."}
                  </p>
                </dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">Description</dt>
                <dd className="mt-1 font-medium">{varSet.description || "—"}</dd>
              </div>
            </dl>
          </CardContent>
        </Card>
      </DetailPageWrapper>

      {/* 弹层：变量新建/编辑、删除确认、版本全文/对比/回滚、集合设置/删除 */}
      <VarFormDialog
        varSetId={varSetId}
        editing={editingVar}
        open={createVarOpen || editingVar !== null}
        onOpenChange={(o) => {
          setCreateVarOpen(o && createVarOpen);
          if (!o) setEditingVar(null);
        }}
      />
      <ConfirmDialog
        open={deletingVarKey !== null}
        onOpenChange={(o) => !o && setDeletingVarKey(null)}
        title="Delete variable"
        description={`Deleting variable ${deletingVarKey ?? ""} takes effect immediately and produces a new version; clients will no longer receive it on their next fetch. You can restore it via version rollback.`}
        confirmLabel="Delete"
        loading={removeVar.pending}
        onConfirm={() =>
          deletingVarKey && removeVar.mutate({ varSetId, key: deletingVarKey })
        }
      />
      <VersionSnapshotDialog
        varSetId={varSetId}
        revision={viewRevision}
        onClose={() => setViewRevision(null)}
      />
      <VersionDiffDialog
        varSetId={varSetId}
        pair={diffPair}
        onClose={() => setDiffPair(null)}
      />
      <ConfirmDialog
        open={rollbackVersion !== null}
        onOpenChange={(o) => !o && setRollbackVersion(null)}
        title={`Roll back to version #${rollbackVersion?.revision ?? ""}`}
        description={`Rolling the set back to version #${rollbackVersion?.revision ?? ""} replaces all current variables with that version's snapshot and produces a new version record; the target version itself remains unchanged.`}
        confirmLabel="Rollback"
        loading={rollbackMut.pending}
        onConfirm={() =>
          rollbackVersion &&
          rollbackMut.mutate({ varSetId, targetRevision: rollbackVersion.revision })
        }
      />
      <VarSetSettingsDialog
        varSet={varSet}
        open={settingsOpen}
        onOpenChange={setSettingsOpen}
      />
      <ConfirmDialog
        open={deleteSetOpen}
        onOpenChange={setDeleteSetOpen}
        title="Delete variable set"
        description={`Deleting variable set ${varSetId} destroys all its variables and the entire version history (irreversible). This action cannot be undone.`}
        confirmLabel="Delete"
        loading={removeSet.pending}
        onConfirm={() => removeSet.mutate({ varSetId })}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// 变量新建/编辑 Dialog（key 编辑时禁改；类型 select 切换值输入控件；json 用
// textarea）。命令化（useCommand×2，§12.3 裁决 8）：值展示形→oneof 提交形
// 收敛与 JSON/整数校验在 descriptor run 内——rejected/conflict 走既有内联错
// 误行（表单容器原样），失败通道 toast；失效 = descriptor invalidates 声明。
// ---------------------------------------------------------------------------

function VarFormDialog({
  varSetId,
  editing,
  open,
  onOpenChange,
}: {
  varSetId: string;
  editing: RuntimeVar | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [key, setKey] = useState("");
  const [kind, setKind] = useState<VarKind>("string");
  const [str, setStr] = useState("");
  const [num, setNum] = useState("");
  const [bool, setBool] = useState("false");
  const [json, setJson] = useState("");
  const [description, setDescription] = useState("");

  // 编辑 descriptor：key 闭包原实体（创建后不可改）；创建态实例惰性（不会被
  // mutate——submit 分流）。
  const settled = (
    result: CommandResult<RuntimeVar> | null,
    failure: ApiError | null
  ) => {
    if (result?.status === "ok") {
      toast.success(editing ? "Variable updated" : "Variable created");
      onOpenChange(false);
    } else if (failure) {
      toast.error(describeApiError(failure));
    }
    // rejected/conflict 不 toast——内联错误行呈现
  };

  const create = useCommand(createVarCommand, {
    onSettled: settled,
  });
  const update = useCommand(editVarDescriptor(varSetId, editing?.key ?? ""), {
    onSettled: settled,
  });

  const pending = editing ? update.pending : create.pending;
  const result = editing ? update.result : create.result;
  const error =
    result != null && result.status !== "ok" ? result.message : null;

  // 仅在打开时同步一次编辑目标（依赖 [open] 而非 editing：背景 refetch 换掉
  // vars 数组对象身份时不应重置正在填写的表单）；命令结果态一并复位。
  useEffect(() => {
    if (!open) return;
    create.reset();
    update.reset();
    if (editing) {
      setKey(editing.key);
      const k = detectKind(editing.value);
      setKind(k);
      setStr(editing.value?.string_value ?? "");
      setNum(
        k === "integer"
          ? String(editing.value?.integer_value ?? "")
          : k === "float"
            ? String(editing.value?.float_value ?? "")
            : ""
      );
      setBool(String(editing.value?.bool_value ?? false));
      setJson(editing.value?.json_value ?? "");
      setDescription(editing.description ?? "");
    } else {
      setKey("");
      setKind("string");
      setStr("");
      setNum("");
      setBool("false");
      setJson("");
      setDescription("");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{editing ? "Edit variable" : "New variable"}</DialogTitle>
          <DialogDescription>
            Type is locked at creation; when editing you can switch type directly
            (provide the full new value).
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            const form = { key, kind, str, num, bool, json, description };
            if (editing) update.mutate({ ...form, varSetId });
            else create.mutate({ ...form, varSetId });
          }}
        >
          <div className="space-y-1.5">
            <Label htmlFor="var-key">Key</Label>
            <Input
              id="var-key"
              value={key}
              onChange={(e) => setKey(e.target.value)}
              required
              disabled={!!editing}
              maxLength={64}
              pattern={KEY_PATTERN}
              placeholder="enable_new_checkout"
              className="font-mono text-xs"
            />
            <p className="text-xs text-muted-foreground">
              Lowercase letters, digits, and underscores; must start with a
              letter or underscore, ≤64 characters
              {editing ? "; immutable after creation" : ""}.
            </p>
          </div>
          <div className="space-y-1.5">
            <Label>Type</Label>
            <Select value={kind} onValueChange={(v) => setKind(v as VarKind)}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {VAR_KINDS.map((k) => (
                  <SelectItem key={k} value={k}>
                    {k}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label>Value</Label>
            {kind === "string" && (
              <Input
                value={str}
                onChange={(e) => setStr(e.target.value)}
                placeholder="Text value"
              />
            )}
            {(kind === "integer" || kind === "float") && (
              <Input
                type="number"
                step={kind === "integer" ? "1" : "any"}
                value={num}
                onChange={(e) => setNum(e.target.value)}
                placeholder={kind === "integer" ? "42" : "3.14"}
              />
            )}
            {kind === "boolean" && (
              <Select value={bool} onValueChange={setBool}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="true">true</SelectItem>
                  <SelectItem value="false">false</SelectItem>
                </SelectContent>
              </Select>
            )}
            {kind === "json" && (
              <textarea
                value={json}
                onChange={(e) => setJson(e.target.value)}
                rows={6}
                placeholder={'{\n  "enabled": true,\n  "ratio": 0.5\n}'}
                className="flex w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 font-mono text-xs"
              />
            )}
            {kind === "json" && (
              <p className="text-xs text-muted-foreground">
                Valid JSON text (nesting ≤100 levels, single value ≤64KB); format
                is validated before submit.
              </p>
            )}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="var-description">Description (optional)</Label>
            <Input
              id="var-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              maxLength={256}
              placeholder="Feature flag for the new checkout page"
            />
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={pending}>
              {pending ? "Saving…" : "Save"}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// 版本全文 Dialog（GetRuntimeVarVersion 全量快照）
// ---------------------------------------------------------------------------

function VersionSnapshotDialog({
  varSetId,
  revision,
  onClose,
}: {
  varSetId: string;
  revision: number | null;
  onClose: () => void;
}) {
  const { projectId } = useAuth();
  const tz = useUserTimezone();
  // 版本快照详情读换轨 useRecord（子视图 kit 化批）：id = revision，未选版本
  //（null → undefined）自动停用（与旧 enabled 同义）；404 → null 空态直出。
  const versionRecord = useRecord(runtimeVarVersionResource, {
    id: revision ?? undefined,
    scope: { projectId: projectId ?? undefined, varSetId: varSetId || undefined },
  });
  const data = versionRecord.data;
  const isLoading = versionRecord.status === "loading";

  return (
    <Dialog open={revision !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Version #{revision} full snapshot</DialogTitle>
          <DialogDescription>
            {data?.version
              ? `${ACTION_LABEL[data.version.action] ?? data.version.action} · ${data.version.summary || "—"} · ${data.version.actor} · ${formatDateTime(data.version.created_at, tz)}`
              : "Snapshot of all variables in this version (expansion of {key:{t,v,d,c}})"}
          </DialogDescription>
        </DialogHeader>
        {isLoading ? (
          <LoadingTable columns={4} />
        ) : !data || data.entries.length === 0 ? (
          <EmptyState title="Empty snapshot" description="No variables in this version" />
        ) : (
          <div className="max-h-[60vh] overflow-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Key</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead>Value</TableHead>
                  <TableHead>Description</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.entries.map((e) => (
                  <TableRow key={e.key}>
                    <TableCell className="font-mono text-xs">{e.key}</TableCell>
                    <TableCell>
                      <TypeBadge kind={detectKind(e.value)} />
                    </TableCell>
                    <TableCell>
                      <pre className="whitespace-pre-wrap break-all font-mono text-xs max-w-[280px]">
                        {formatValueText(e.value)}
                      </pre>
                    </TableCell>
                    <TableCell className="text-xs">{e.description || "—"}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// 版本对比 Dialog（拉两版全文，前端逐 key 新旧值对照；服务端无 diff API）
// ---------------------------------------------------------------------------

interface DiffRow {
  key: string;
  a?: RuntimeVarVersionEntry;
  b?: RuntimeVarVersionEntry;
  status: "added" | "removed" | "changed";
}

// valueKey 以「类型 + 规范化文本」为等价判据（integer 1 与 float 1 视为不同，
// 与存储模型的 value_type 锚点一致）。
function entryValueKey(e: RuntimeVarVersionEntry): string {
  const v = e.value;
  if (!v) return "none";
  if (v.string_value !== undefined) return `string:${v.string_value}`;
  if (v.integer_value !== undefined) return `integer:${v.integer_value}`;
  if (v.float_value !== undefined) return `float:${v.float_value}`;
  if (v.bool_value !== undefined) return `boolean:${v.bool_value}`;
  if (v.json_value !== undefined) return `json:${v.json_value}`;
  return "none";
}

function diffEntries(
  a: RuntimeVarVersionEntry[],
  b: RuntimeVarVersionEntry[]
): { rows: DiffRow[]; unchanged: number } {
  const mapA = new Map(a.map((e) => [e.key, e]));
  const mapB = new Map(b.map((e) => [e.key, e]));
  const keys = Array.from(new Set([...mapA.keys(), ...mapB.keys()])).sort();
  const rows: DiffRow[] = [];
  let unchanged = 0;
  for (const key of keys) {
    const ea = mapA.get(key);
    const eb = mapB.get(key);
    if (!ea) {
      rows.push({ key, a: ea, b: eb, status: "added" });
    } else if (!eb) {
      rows.push({ key, a: ea, b: eb, status: "removed" });
    } else if (entryValueKey(ea) === entryValueKey(eb)) {
      unchanged += 1;
    } else {
      rows.push({ key, a: ea, b: eb, status: "changed" });
    }
  }
  return { rows, unchanged };
}

function DiffValueCell({ entry }: { entry?: RuntimeVarVersionEntry }) {
  if (!entry) return <span className="text-muted-foreground">—</span>;
  return (
    <div className="space-y-1">
      <TypeBadge kind={detectKind(entry.value)} />
      <pre className="whitespace-pre-wrap break-all font-mono text-xs max-w-[280px]">
        {formatValueText(entry.value)}
      </pre>
    </div>
  );
}

function VersionDiffDialog({
  varSetId,
  pair,
  onClose,
}: {
  varSetId: string;
  pair: { a: number; b: number } | null;
  onClose: () => void;
}) {
  const { projectId } = useAuth();
  // 两侧快照读换轨 useRecord（子视图 kit 化批）：revision 类参数
  // `pair?.a ?? undefined`——未选对比对（null）自动停用（与旧 enabled 同义）。
  const aRecord = useRecord(runtimeVarVersionResource, {
    id: pair?.a ?? undefined,
    scope: { projectId: projectId ?? undefined, varSetId: varSetId || undefined },
  });
  const bRecord = useRecord(runtimeVarVersionResource, {
    id: pair?.b ?? undefined,
    scope: { projectId: projectId ?? undefined, varSetId: varSetId || undefined },
  });

  const diff = useMemo(() => {
    if (!aRecord.data || !bRecord.data) return null;
    return diffEntries(aRecord.data.entries, bRecord.data.entries);
  }, [aRecord.data, bRecord.data]);

  const isLoading = aRecord.status === "loading" || bRecord.status === "loading";
  const counts = useMemo(() => {
    if (!diff) return null;
    return {
      added: diff.rows.filter((r) => r.status === "added").length,
      removed: diff.rows.filter((r) => r.status === "removed").length,
      changed: diff.rows.filter((r) => r.status === "changed").length,
    };
  }, [diff]);

  return (
    <Dialog open={pair !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Version diff</DialogTitle>
          <DialogDescription>
            {pair
              ? `Version #${pair.a} → version #${pair.b}${counts ? `: ${counts.added} added · ${counts.removed} removed · ${counts.changed} changed · ${diff?.unchanged ?? 0} keys unchanged` : ""}`
              : "Key-by-key old/new value comparison (computed client-side)"}
          </DialogDescription>
        </DialogHeader>
        {isLoading ? (
          <LoadingTable columns={4} />
        ) : !diff ? (
          <EmptyState title="Failed to load diff" description="Failed to fetch version snapshots" />
        ) : diff.rows.length === 0 ? (
          <EmptyState title="The two versions are identical" description="No keys were added, removed, or changed" />
        ) : (
          <div className="max-h-[60vh] overflow-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Key</TableHead>
                  <TableHead>Old value (#{pair?.a})</TableHead>
                  <TableHead>New value (#{pair?.b})</TableHead>
                  <TableHead>Change</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {diff.rows.map((row) => (
                  <TableRow key={row.key}>
                    <TableCell className="font-mono text-xs align-top">
                      {row.key}
                    </TableCell>
                    <TableCell className="align-top">
                      <DiffValueCell entry={row.a} />
                    </TableCell>
                    <TableCell className="align-top">
                      <DiffValueCell entry={row.b} />
                    </TableCell>
                    <TableCell className="align-top">
                      {row.status === "added" && <Badge>Added</Badge>}
                      {row.status === "removed" && (
                        <Badge variant="destructive">Removed</Badge>
                      )}
                      {row.status === "changed" && (
                        <Badge variant="secondary">Changed</Badge>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// 集合设置 Dialog（visibility 双向切换均强确认，D15；元数据变更不入版本链，D10）
// ---------------------------------------------------------------------------

function VarSetSettingsDialog({
  varSet,
  open,
  onOpenChange,
}: {
  varSet: VarSet;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [visibility, setVisibility] = useState<VarSetVisibilityInput>(
    toVisibility(varSet.visibility)
  );
  const [description, setDescription] = useState(varSet.description);
  const [confirmOpen, setConfirmOpen] = useState(false);

  useEffect(() => {
    if (open) {
      setVisibility(toVisibility(varSet.visibility));
      setDescription(varSet.description);
      setConfirmOpen(false);
    }
  }, [open, varSet]);

  // 命令化：visibility diff（未变化不发送）归 descriptor（闭包原实体，LWW
  // 无 OCC 前置守卫）；失效 = descriptor invalidates（var-sets 全 kind——列表
  // 与详情 record 一并刷新，§12.10-② 存量前缀桥退役）；结果分支无表单容器
  // → toast。
  const save = useCommand(editVarSetDescriptor(varSet), {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Set settings updated");
        onOpenChange(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const visibilityChanged = visibility !== toVisibility(varSet.visibility);
  const switchingToPublic = visibility === "VAR_SET_VISIBILITY_PUBLIC";

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Edit set settings</DialogTitle>
            <DialogDescription>
              Metadata changes do not create version records and do not affect
              client ETags.
            </DialogDescription>
          </DialogHeader>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              if (visibilityChanged) {
                setConfirmOpen(true);
                return;
              }
              save.mutate({ visibility, description });
            }}
          >
            <div className="space-y-1.5">
              <Label>Visibility</Label>
              <Select
                value={visibility}
                onValueChange={(v) => setVisibility(v as VarSetVisibilityInput)}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="VAR_SET_VISIBILITY_PUBLIC">
                    Public — readable anonymously by any client holding the project_id
                  </SelectItem>
                  <SelectItem value="VAR_SET_VISIBILITY_PRIVATE">
                    Private — visible to signed-in project users only
                  </SelectItem>
                </SelectContent>
              </Select>
              {visibilityChanged && (
                <p className="text-xs text-amber-600">
                  Changing visibility is a destructive operation; a second
                  confirmation is required before saving.
                </p>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="settings-description">Description</Label>
              <Input
                id="settings-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                maxLength={256}
                placeholder="Client feature flags and thresholds"
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={save.pending}>
                {save.pending ? "Saving…" : "Save"}
              </Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
      {switchingToPublic ? (
        <ConfirmDialog
          open={confirmOpen}
          onOpenChange={setConfirmOpen}
          title="Switch to public"
          description="After switching, the entire variable set (including all current values) becomes readable by any client holding the project_id, no sign-in required. Confirm that it contains no sensitive information."
          confirmLabel="Switch to public"
          loading={save.pending}
          onConfirm={() => {
            setConfirmOpen(false);
            save.mutate({ visibility, description });
          }}
        />
      ) : (
        <ConfirmDialog
          open={confirmOpen}
          onOpenChange={setConfirmOpen}
          title="Switch to private"
          description="After switching, anonymous clients immediately receive a 404; the app falls back to built-in defaults or locally cached last-known-good configuration until the user signs in."
          confirmLabel="Switch to private"
          loading={save.pending}
          onConfirm={() => {
            setConfirmOpen(false);
            save.mutate({ visibility, description });
          }}
        />
      )}
    </>
  );
}
