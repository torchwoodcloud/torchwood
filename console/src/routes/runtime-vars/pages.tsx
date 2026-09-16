import { useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
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
  createVar,
  createVarSet,
  deleteVar,
  deleteVarSet,
  getVarSet,
  getVersion,
  listVarSets,
  listVars,
  listVersions,
  rollback,
  updateVar,
  updateVarSet,
  type RuntimeVar,
  type RuntimeVarValue,
  type RuntimeVarValueInput,
  type RuntimeVarVersion,
  type RuntimeVarVersionEntry,
  type VarSet,
  type VarSetVisibilityInput,
} from "@/api/runtimeVars";
import { useAuth } from "@/hooks/useAuth";
import { isPlatformAdmin, useAdminRole } from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { EmptyState } from "@/components/EmptyState";
import { LoadingTable } from "@/components/LoadingTable";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import type { ColumnDef } from "@/components/list/DataTable";
import {
  DetailPageWrapper,
  DetailSkeleton,
  NotFound,
} from "@/components/resource/shared";
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
const SAFE_INTEGER_LIMIT = 9007199254740991;

const ACTION_LABEL: Record<string, string> = {
  RUNTIME_VAR_VERSION_ACTION_CREATE: "创建",
  RUNTIME_VAR_VERSION_ACTION_UPDATE: "更新",
  RUNTIME_VAR_VERSION_ACTION_DELETE: "删除",
  RUNTIME_VAR_VERSION_ACTION_ROLLBACK: "回滚",
};

type VarKind = "string" | "integer" | "float" | "boolean" | "json";
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

// buildValueInput 前端构造 oneof 输入并做格式校验（json 的 JSON.parse 校验是
// 交接要点；integer 限 ±2^53−1 与 protovalidate 对齐）。
function buildValueInput(
  kind: VarKind,
  s: { str: string; num: string; bool: boolean; json: string }
): { value?: RuntimeVarValueInput; error?: string } {
  switch (kind) {
    case "string":
      return { value: { string_value: s.str } };
    case "integer": {
      if (!s.num.trim()) return { error: "整数不能为空" };
      const n = Number(s.num);
      if (!Number.isInteger(n)) return { error: "不是合法的整数" };
      if (Math.abs(n) > SAFE_INTEGER_LIMIT) {
        return { error: "超出安全整数范围（±2^53−1）" };
      }
      return { value: { integer_value: n } };
    }
    case "float": {
      if (!s.num.trim()) return { error: "数值不能为空" };
      const n = Number(s.num);
      if (!Number.isFinite(n)) return { error: "不是合法的数值" };
      return { value: { float_value: n } };
    }
    case "boolean":
      return { value: { bool_value: s.bool } };
    case "json": {
      if (!s.json.trim()) return { error: "JSON 不能为空" };
      try {
        JSON.parse(s.json);
      } catch (e) {
        return {
          error: `JSON 格式不合法：${e instanceof Error ? e.message : String(e)}`,
        };
      }
      return { value: { json_value: s.json } };
    }
  }
}

// SecretWarningBanner 两页顶部固定警示条（§2.7：public 集 = 持 project_id 的
// 客户端匿名可读；private 集 = 登录用户可见；禁止存放机密）。
function SecretWarningBanner() {
  return (
    <div className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 px-4 py-3 text-sm text-amber-700 dark:text-amber-400">
      <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
      <p>
        变量对持有 project_id 的客户端公开（public 集合）或仅登录用户可见（private
        集合），禁止存放机密（API Key、密码、私钥等）。
      </p>
    </div>
  );
}

function VisibilityBadge({ visibility }: { visibility: string }) {
  const isPublic = visibility !== "VAR_SET_VISIBILITY_PRIVATE";
  return (
    <Badge variant={isPublic ? "default" : "secondary"} title={visibility}>
      {isPublic ? "公开" : "私有"}
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

// useInvalidateVarSet 集中失效该集合相关全部查询（var 写会 bump revision，
// var-sets 列表的 var_count/footprint/updated_at 也会变）。
function useInvalidateVarSet(varSetId: string) {
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  return () => {
    queryClient.invalidateQueries({ queryKey: ["runtime-var-sets", projectId] });
    queryClient.invalidateQueries({
      queryKey: ["runtime-var-set", projectId, varSetId],
    });
    queryClient.invalidateQueries({
      queryKey: ["runtime-vars", projectId, varSetId],
    });
    queryClient.invalidateQueries({
      queryKey: ["runtime-var-versions", projectId, varSetId],
    });
    queryClient.invalidateQueries({
      queryKey: ["runtime-var-version", projectId, varSetId],
    });
  };
}

// ---------------------------------------------------------------------------
// 列表页
// ---------------------------------------------------------------------------

export function VarSetsListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const [createOpen, setCreateOpen] = useState(false);
  const platformAdmin = isPlatformAdmin(role);

  const { data: sets = [], isLoading } = useQuery({
    queryKey: ["runtime-var-sets", projectId],
    queryFn: listVarSets,
    enabled: !!projectId,
  });

  const columns: ColumnDef<VarSet>[] = [
    {
      key: "var_set_id",
      header: "集合 ID",
      className: "font-mono text-xs",
      cell: (s) => s.var_set_id,
    },
    {
      key: "visibility",
      header: "可见性",
      cell: (s) => <VisibilityBadge visibility={s.visibility} />,
    },
    {
      key: "var_count",
      header: "变量数",
      cell: (s) => `${s.var_count} / ${VAR_COUNT_LIMIT}`,
    },
    {
      key: "revision",
      header: "当前版本",
      cell: (s) => `#${s.revision}`,
    },
    {
      key: "footprint",
      header: "占用（上限 1 MB）",
      cell: (s) => <FootprintCell bytes={s.total_bytes} />,
    },
    {
      key: "updated_at",
      header: "更新时间",
      cell: (s) => formatDateTime(s.updated_at, tz),
    },
  ];

  return (
    <div className="space-y-6">
      <SecretWarningBanner />
      <ResourceListPage
        title="Runtime Vars"
        description="管理项目级运行时配置：类型化变量集合、可见性与版本回滚"
        searchPlaceholder="搜索集合 ID 或描述..."
        isLoading={isLoading}
        items={sets}
        columns={columns}
        getSearchText={(s) => `${s.var_set_id} ${s.description}`}
        detailPath={(s) => `/console/runtime-vars/${s.var_set_id}`}
        toolbarActions={
          platformAdmin ? (
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="h-4 w-4 mr-2" />
              新建变量集
            </Button>
          ) : undefined
        }
        emptyTitle="暂无变量集"
        emptyDescription="创建第一个变量集，向客户端下发功能开关、文案、阈值等运行时配置"
        emptyAction={
          platformAdmin ? (
            <Button onClick={() => setCreateOpen(true)}>新建变量集</Button>
          ) : undefined
        }
      />
      <VarSetCreateDialog open={createOpen} onOpenChange={setCreateOpen} />
    </div>
  );
}

// VarSetCreateDialog 新建集合（var_set_id 创建后不可改，D9）。
function VarSetCreateDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
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

  const mutation = useMutation({
    mutationFn: () =>
      createVarSet({
        var_set_id: varSetId,
        visibility,
        description,
      }),
    onSuccess: () => {
      toast.success("变量集已创建");
      queryClient.invalidateQueries({
        queryKey: ["runtime-var-sets", projectId],
      });
      onOpenChange(false);
    },
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>新建变量集</DialogTitle>
          <DialogDescription>
            集合是变量的容器；创建后不可重命名（重建 + 回滚替代）。
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            mutation.mutate();
          }}
        >
          <div className="space-y-1.5">
            <Label htmlFor="var-set-id">集合 ID</Label>
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
              小写字母/数字/下划线，字母或下划线开头，≤40 字符；创建后不可修改。
            </p>
          </div>
          <div className="space-y-1.5">
            <Label>可见性</Label>
            <Select value={visibility} onValueChange={(v) => setVisibility(v as VarSetVisibilityInput)}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="VAR_SET_VISIBILITY_PUBLIC">
                  公开（public）— 任何持有 project_id 的客户端可匿名读取
                </SelectItem>
                <SelectItem value="VAR_SET_VISIBILITY_PRIVATE">
                  私有（private）— 仅项目登录用户可见
                </SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="var-set-description">描述（可选）</Label>
            <Input
              id="var-set-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              maxLength={256}
              placeholder="客户端功能开关与阈值"
            />
          </div>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "创建中…" : "创建"}
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
  const { varSetId = "" } = useParams<{ varSetId: string }>();
  const navigate = useNavigate();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const invalidate = useInvalidateVarSet(varSetId);
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

  const varSetQuery = useQuery({
    queryKey: ["runtime-var-set", projectId, varSetId],
    queryFn: () => getVarSet(varSetId),
    enabled: !!projectId && !!varSetId,
  });
  const varsQuery = useQuery({
    queryKey: ["runtime-vars", projectId, varSetId],
    queryFn: () => listVars(varSetId),
    enabled: !!projectId && !!varSetId,
  });
  const versionsQuery = useQuery({
    queryKey: ["runtime-var-versions", projectId, varSetId],
    queryFn: () => listVersions(varSetId),
    enabled: !!projectId && !!varSetId,
  });

  const vars = varsQuery.data ?? [];
  // 用 query 原始引用（跨渲染稳定）做 effect 依赖；`?? []` 的派生数组每次
  // 渲染都是新身份，直接进 deps 会让 effect 每帧重跑。
  const versionsRaw = versionsQuery.data;
  const versions = versionsRaw ?? [];

  const removeVar = useMutation({
    mutationFn: (key: string) => deleteVar(varSetId, key),
    onSuccess: () => {
      toast.success("变量已删除");
      invalidate();
      setDeletingVarKey(null);
    },
  });

  const removeSet = useMutation({
    mutationFn: () => deleteVarSet(varSetId),
    onSuccess: () => {
      toast.success("变量集已删除");
      queryClient.invalidateQueries({
        queryKey: ["runtime-var-sets", projectId],
      });
      navigate("/console/runtime-vars");
    },
  });

  const rollbackMut = useMutation({
    mutationFn: (targetRevision: number) => rollback(varSetId, targetRevision),
    onSuccess: (created, target) => {
      toast.success(`已回滚到版本 #${target}，产生新版本 #${created.revision}`);
      invalidate();
      setRollbackVersion(null);
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
    if (!versionsRaw || versionsRaw.length === 0 || diffA || diffB) return;
    const revs = versionsRaw.map((v) => v.revision);
    setDiffA(String(Math.min(...revs)));
    setDiffB(String(Math.max(...revs)));
  }, [versionsRaw, diffA, diffB]);

  const diffReady =
    !!diffA && !!diffB && diffA !== diffB && versions.length >= 2;

  if (varSetQuery.isLoading) return <DetailSkeleton />;
  const varSet = varSetQuery.data;
  if (!varSet) return <NotFound backTo="/console/runtime-vars" />;

  return (
    <div className="space-y-6">
      <SecretWarningBanner />
      <DetailPageWrapper
        title={varSet.var_set_id}
        description={varSet.description || "运行时变量集"}
        backTo="/console/runtime-vars"
      >
        <Card>
          <CardContent className="pt-6">
            <dl className="grid gap-4 sm:grid-cols-3">
              <div>
                <dt className="text-sm text-muted-foreground">可见性</dt>
                <dd className="mt-1">
                  <VisibilityBadge visibility={varSet.visibility} />
                </dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">当前版本</dt>
                <dd className="mt-1 font-medium">#{varSet.revision}</dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">变量数</dt>
                <dd className="mt-1 font-medium">
                  {varSet.var_count} / {VAR_COUNT_LIMIT}
                </dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">总值尺寸</dt>
                <dd className="mt-1 font-medium">
                  {formatBytes(varSet.total_bytes)} / 1 MB
                </dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">ETag</dt>
                <dd className="mt-1 font-mono text-sm break-all">{varSet.etag}</dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">更新时间</dt>
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
            <CardTitle>变量（{vars.length}）</CardTitle>
            {platformAdmin && (
              <Button size="sm" onClick={() => setCreateVarOpen(true)}>
                <Plus className="h-4 w-4 mr-2" />
                新建变量
              </Button>
            )}
          </CardHeader>
          <CardContent>
            {varsQuery.isLoading ? (
              <LoadingTable columns={5} />
            ) : vars.length === 0 ? (
              <EmptyState
                title="暂无变量"
                description="新建类型化变量，客户端将按集合全量拉取（ETag 轮询）"
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Key</TableHead>
                    <TableHead>类型</TableHead>
                    <TableHead>值</TableHead>
                    <TableHead>更新时间</TableHead>
                    {platformAdmin && <TableHead className="text-right">操作</TableHead>}
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
                                title="编辑"
                              >
                                <Pencil className="h-4 w-4" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                onClick={() => setDeletingVarKey(v.key)}
                                title="删除"
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
            <CardTitle>版本历史（{versions.length}，保留最近 50 版）</CardTitle>
            <div className="flex flex-wrap items-end gap-2">
              <div className="space-y-1">
                <Label className="text-xs text-muted-foreground">旧版本（基准）</Label>
                <Select value={diffA} onValueChange={setDiffA}>
                  <SelectTrigger className="w-36">
                    <SelectValue placeholder="选择版本" />
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
                <Label className="text-xs text-muted-foreground">新版本（对比）</Label>
                <Select value={diffB} onValueChange={setDiffB}>
                  <SelectTrigger className="w-36">
                    <SelectValue placeholder="选择版本" />
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
                对比两版
              </Button>
            </div>
          </CardHeader>
          <CardContent>
            {versionsQuery.isLoading ? (
              <LoadingTable columns={6} />
            ) : versions.length === 0 ? (
              <EmptyState
                title="暂无版本"
                description="变量增删改与回滚会自动产生版本记录"
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>版本</TableHead>
                    <TableHead>动作</TableHead>
                    <TableHead>摘要</TableHead>
                    <TableHead>操作者</TableHead>
                    <TableHead>时间</TableHead>
                    {platformAdmin && <TableHead className="text-right">操作</TableHead>}
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
                                title="查看全文"
                              >
                                <Eye className="h-4 w-4" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                disabled={isCurrent}
                                onClick={() => setRollbackVersion(v)}
                                title={isCurrent ? "已是当前版本" : "回滚到此版本"}
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
            <CardTitle>集合设置</CardTitle>
            {platformAdmin && (
              <div className="flex gap-2">
                <Button variant="outline" size="sm" onClick={() => setSettingsOpen(true)}>
                  <Pencil className="h-4 w-4 mr-2" />
                  编辑设置
                </Button>
                <Button variant="destructive" size="sm" onClick={() => setDeleteSetOpen(true)}>
                  <Trash2 className="h-4 w-4 mr-2" />
                  删除集合
                </Button>
              </div>
            )}
          </CardHeader>
          <CardContent>
            <dl className="grid gap-4 sm:grid-cols-2">
              <div>
                <dt className="text-sm text-muted-foreground">可见性</dt>
                <dd className="mt-1">
                  <VisibilityBadge visibility={varSet.visibility} />
                  <p className="mt-1 text-xs text-muted-foreground">
                    {varSet.visibility === "VAR_SET_VISIBILITY_PRIVATE"
                      ? "私有：仅项目登录用户可见，匿名客户端收到 404。"
                      : "公开：任何持有 project_id 的客户端可匿名读取。"}
                  </p>
                </dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">描述</dt>
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
        title="确认删除变量"
        description={`删除变量 ${deletingVarKey ?? ""} 会立即生效并产生一个新版本；客户端下次拉取将不再收到该变量。可通过版本回滚恢复。`}
        confirmLabel="删除"
        loading={removeVar.isPending}
        onConfirm={() => deletingVarKey && removeVar.mutate(deletingVarKey)}
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
        title={`确认回滚到版本 #${rollbackVersion?.revision ?? ""}`}
        description={`将集合回滚到版本 #${rollbackVersion?.revision ?? ""}：当前全部变量会被该版本的快照整替，同时产生一个新的版本记录；目标版本本身保持不变。`}
        confirmLabel="回滚"
        loading={rollbackMut.isPending}
        onConfirm={() =>
          rollbackVersion && rollbackMut.mutate(rollbackVersion.revision)
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
        title="确认删除变量集"
        description={`删除变量集 ${varSetId} 将连同全部变量与全部版本历史一起销毁（不可恢复）。此操作不可撤销。`}
        confirmLabel="删除"
        loading={removeSet.isPending}
        onConfirm={() => removeSet.mutate()}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// 变量新建/编辑 Dialog（key 编辑时禁改；类型 select 切换值输入控件；json 用
// textarea + 前端 JSON.parse 校验）
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
  const invalidate = useInvalidateVarSet(varSetId);
  const [key, setKey] = useState("");
  const [kind, setKind] = useState<VarKind>("string");
  const [str, setStr] = useState("");
  const [num, setNum] = useState("");
  const [bool, setBool] = useState("false");
  const [json, setJson] = useState("");
  const [description, setDescription] = useState("");
  const [error, setError] = useState<string | null>(null);

  // 仅在打开时同步一次编辑目标（依赖 [open] 而非 editing：背景 refetch 换掉
  // vars 数组对象身份时不应重置正在填写的表单）。
  useEffect(() => {
    if (!open) return;
    setError(null);
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

  const mutation = useMutation({
    mutationFn: (value: RuntimeVarValueInput) =>
      editing
        ? updateVar(varSetId, editing.key, { value, description })
        : createVar(varSetId, { key, value, description }),
    onSuccess: () => {
      toast.success(editing ? "变量已更新" : "变量已创建");
      invalidate();
      onOpenChange(false);
    },
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{editing ? "修改变量" : "新建变量"}</DialogTitle>
          <DialogDescription>
            类型创建时锁定，修改变更时可直接切换类型（携带完整新值）。
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            const built = buildValueInput(kind, {
              str,
              num,
              bool: bool === "true",
              json,
            });
            if (!built.value) {
              setError(built.error ?? "值不合法");
              return;
            }
            setError(null);
            mutation.mutate(built.value);
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
              小写字母/数字/下划线，字母或下划线开头，≤64 字符
              {editing ? "；创建后不可修改" : ""}。
            </p>
          </div>
          <div className="space-y-1.5">
            <Label>类型</Label>
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
            <Label>值</Label>
            {kind === "string" && (
              <Input
                value={str}
                onChange={(e) => setStr(e.target.value)}
                placeholder="文本值"
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
                合法 JSON 文本（嵌套 ≤100 层，单值 ≤64KB），提交前会做格式校验。
              </p>
            )}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="var-description">描述（可选）</Label>
            <Input
              id="var-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              maxLength={256}
              placeholder="新版结账页功能开关"
            />
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "保存中…" : "保存"}
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
  const { data, isLoading } = useQuery({
    queryKey: ["runtime-var-version", projectId, varSetId, revision],
    queryFn: () => getVersion(varSetId, revision!),
    enabled: revision !== null,
  });

  return (
    <Dialog open={revision !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>版本 #{revision} 全文快照</DialogTitle>
          <DialogDescription>
            {data?.version
              ? `${ACTION_LABEL[data.version.action] ?? data.version.action} · ${data.version.summary || "—"} · ${data.version.actor} · ${formatDateTime(data.version.created_at, tz)}`
              : "该版本全部变量的快照（{key:{t,v,d,c}} 的展开）"}
          </DialogDescription>
        </DialogHeader>
        {isLoading ? (
          <LoadingTable columns={4} />
        ) : !data || data.entries.length === 0 ? (
          <EmptyState title="空快照" description="该版本没有任何变量" />
        ) : (
          <div className="max-h-[60vh] overflow-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Key</TableHead>
                  <TableHead>类型</TableHead>
                  <TableHead>值</TableHead>
                  <TableHead>描述</TableHead>
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
  const aQuery = useQuery({
    queryKey: ["runtime-var-version", projectId, varSetId, pair?.a],
    queryFn: () => getVersion(varSetId, pair!.a),
    enabled: pair !== null,
  });
  const bQuery = useQuery({
    queryKey: ["runtime-var-version", projectId, varSetId, pair?.b],
    queryFn: () => getVersion(varSetId, pair!.b),
    enabled: pair !== null,
  });

  const diff = useMemo(() => {
    if (!aQuery.data || !bQuery.data) return null;
    return diffEntries(aQuery.data.entries, bQuery.data.entries);
  }, [aQuery.data, bQuery.data]);

  const isLoading = aQuery.isLoading || bQuery.isLoading;
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
          <DialogTitle>版本对比</DialogTitle>
          <DialogDescription>
            {pair
              ? `版本 #${pair.a} → 版本 #${pair.b}${counts ? `：新增 ${counts.added} · 删除 ${counts.removed} · 变更 ${counts.changed} · 未变 ${diff?.unchanged ?? 0} 个键` : ""}`
              : "逐 key 新旧值对照（前端计算）"}
          </DialogDescription>
        </DialogHeader>
        {isLoading ? (
          <LoadingTable columns={4} />
        ) : !diff ? (
          <EmptyState title="无法加载对比" description="版本快照获取失败" />
        ) : diff.rows.length === 0 ? (
          <EmptyState title="两版本内容完全一致" description="没有任何键发生新增、删除或变更" />
        ) : (
          <div className="max-h-[60vh] overflow-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Key</TableHead>
                  <TableHead>旧值（#{pair?.a}）</TableHead>
                  <TableHead>新值（#{pair?.b}）</TableHead>
                  <TableHead>变化</TableHead>
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
                      {row.status === "added" && <Badge>新增</Badge>}
                      {row.status === "removed" && (
                        <Badge variant="destructive">删除</Badge>
                      )}
                      {row.status === "changed" && (
                        <Badge variant="secondary">变更</Badge>
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
  const invalidate = useInvalidateVarSet(varSet.var_set_id);
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

  const save = useMutation({
    mutationFn: () =>
      updateVarSet(varSet.var_set_id, {
        // proto3 optional：未变化的 visibility 不发送 = 不修改。
        visibility:
          visibility !== toVisibility(varSet.visibility) ? visibility : undefined,
        description,
      }),
    onSuccess: () => {
      toast.success("集合设置已更新");
      invalidate();
      onOpenChange(false);
    },
  });

  const visibilityChanged = visibility !== toVisibility(varSet.visibility);
  const switchingToPublic = visibility === "VAR_SET_VISIBILITY_PUBLIC";

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>编辑集合设置</DialogTitle>
            <DialogDescription>
              元数据变更不产生版本记录、不影响客户端 ETag。
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
              save.mutate();
            }}
          >
            <div className="space-y-1.5">
              <Label>可见性</Label>
              <Select
                value={visibility}
                onValueChange={(v) => setVisibility(v as VarSetVisibilityInput)}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="VAR_SET_VISIBILITY_PUBLIC">
                    公开（public）— 任何持有 project_id 的客户端可匿名读取
                  </SelectItem>
                  <SelectItem value="VAR_SET_VISIBILITY_PRIVATE">
                    私有（private）— 仅项目登录用户可见
                  </SelectItem>
                </SelectContent>
              </Select>
              {visibilityChanged && (
                <p className="text-xs text-amber-600">
                  可见性切换是破坏性操作，保存前需二次确认。
                </p>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="settings-description">描述</Label>
              <Input
                id="settings-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                maxLength={256}
                placeholder="客户端功能开关与阈值"
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
                取消
              </Button>
              <Button type="submit" disabled={save.isPending}>
                {save.isPending ? "保存中…" : "保存"}
              </Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
      {switchingToPublic ? (
        <ConfirmDialog
          open={confirmOpen}
          onOpenChange={setConfirmOpen}
          title="确认切换为公开"
          description="切换后整个变量集（含全部当前值）将对任何持有 project_id 的客户端公开，无需登录即可读取。请确认其中不含敏感信息。"
          confirmLabel="切换为公开"
          loading={save.isPending}
          onConfirm={() => {
            setConfirmOpen(false);
            save.mutate();
          }}
        />
      ) : (
        <ConfirmDialog
          open={confirmOpen}
          onOpenChange={setConfirmOpen}
          title="确认切换为私有"
          description="切换后匿名客户端将立即收到 404，App 会回退到内置默认值或本地缓存的 last-known-good 配置，直到用户登录。"
          confirmLabel="切换为私有"
          loading={save.isPending}
          onConfirm={() => {
            setConfirmOpen(false);
            save.mutate();
          }}
        />
      )}
    </>
  );
}
