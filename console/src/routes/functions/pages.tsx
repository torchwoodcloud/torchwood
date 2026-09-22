import { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, Play, UploadCloud, Trash2, GitBranch, Container } from "lucide-react";
import {
  listFunctions,
  getFunction,
  createFunction,
  updateFunction,
  deleteFunction,
  listRuntimes,
  listSpecifications,
  listDeployments,
  uploadDeployment,
  createDeploymentGit,
  createDeploymentImage,
  deleteDeployment,
  getVariables,
  setVariables,
  setFunctionScopes,
  SECRET_MASK,
  createExecution,
  listExecutions,
  type FunctionItem,
  type Deployment,
  type Execution,
  type RuntimeInfo,
  type Variable,
} from "@/api/functions";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { ListPaginationKeyset } from "@/components/list/ListToolbar";
import { useServerPaging } from "@/hooks/useServerPaging";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
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
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DetailPageWrapper,
  DetailGrid,
  DetailSkeleton,
  NotFound,
  DeleteButton,
  RowDeleteButton,
  BulkDeleteButton,
} from "@/components/resource/shared";
import { useAuth } from "@/hooks/useAuth";
import { useUserTimezone } from "@/hooks/useTimezone";
import { useAdminRole, canWrite, isPlatformAdmin } from "@/hooks/useAdminRole";
import { formatDateTime } from "@/lib/datetime";
import type { ColumnDef } from "@/components/list/DataTable";
import { FunctionTriggersCard } from "./triggers-card";
import { FunctionClientPolicyCard } from "./client-policy-card";
import { FunctionPoolPolicyCard } from "./pool-policy-card";

// ——运行时指定（functions-runtime-selection.md §1/§6）的展示助手——

// runtimeStatusLabel 下拉项里的生命周期标注：eol 禁选、deprecated 可选
// 但提示、active 显示缺省标记。
function runtimeStatusLabel(r: RuntimeInfo): string {
  if (r.status === "eol") return "已 EOL";
  if (r.status === "deprecated") return "即将弃用";
  return r.is_default ? "默认" : "";
}

// runtimeBadge 非 active runtime 的行内徽章（列表/详情/部署快照共用）。
function runtimeBadge(r?: RuntimeInfo) {
  if (!r || r.status === "active") return null;
  return r.status === "eol" ? (
    <Badge variant="destructive" className="px-1 py-0 text-[10px]">
      EOL
    </Badge>
  ) : (
    <Badge variant="outline" className="px-1 py-0 text-[10px]">
      弃用
    </Badge>
  );
}

// 模块级 columns 工厂注入管理员时区偏好与运行时表（eol 徽章需要按
// runtime ID 查表）；runtimes 引用稳定后 useMemo 化避免列表重渲染。
const functionColumns = (
  tz: string,
  runtimes: RuntimeInfo[]
): ColumnDef<FunctionItem>[] => [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[140px] truncate",
    cell: (f) => f.id,
  },
  {
    key: "name",
    header: "名称",
    cell: (f) => f.name,
  },
  {
    key: "runtime",
    header: "Runtime",
    cell: (f) => (
      <span className="flex items-center gap-1">
        {f.runtime}
        {runtimeBadge(runtimes.find((r) => r.id === f.runtime))}
      </span>
    ),
  },
  {
    key: "enabled",
    header: "状态",
    cell: (f) =>
      f.enabled ? (
        <Badge variant="secondary">启用</Badge>
      ) : (
        <Badge variant="destructive">禁用</Badge>
      ),
  },
  {
    key: "created",
    header: "创建时间",
    cell: (f) => formatDateTime(f.created_at, tz),
  },
];

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + " " + sizes[i];
}

export function FunctionsListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [name, setName] = useState("");
  // runtime 置空 = 未选择：由运行时表的 is_default 项回填（不再硬编码
  // node-18.0——eol 项不可作为新函数缺省，functions-runtime-selection.md §1）。
  const [runtime, setRuntime] = useState("");
  const [timeoutSeconds, setTimeoutSeconds] = useState("15");
  const [spec, setSpec] = useState("shared-1x");
  const [enabled, setEnabled] = useState(true);
  const writeable = canWrite(role);

  const paging = useServerPaging();
  const { data, isLoading } = useQuery({
    queryKey: ["functions", projectId, paging.pageSize, paging.pageToken],
    queryFn: () => listFunctions({ pageSize: paging.pageSize, pageToken: paging.pageToken }),
    enabled: !!projectId,
    placeholderData: (prev) => prev,
  });
  const functions = data?.rows ?? [];

  const { data: runtimes = [] } = useQuery({
    queryKey: ["functions-runtimes"],
    queryFn: listRuntimes,
  });

  const { data: specifications = [] } = useQuery({
    queryKey: ["functions-specifications"],
    queryFn: listSpecifications,
  });

  const defaultRuntimeId =
    runtimes.find((r) => r.is_default)?.id ??
    runtimes.find((r) => r.status === "active")?.id ??
    "";
  const effectiveRuntime = runtime || defaultRuntimeId;

  const columns = useMemo(() => functionColumns(tz, runtimes), [tz, runtimes]);

  const remove = useMutation({
    mutationFn: (id: string) => deleteFunction(id),
    onSuccess: () => {
      toast.success("函数已删除");
      queryClient.invalidateQueries({ queryKey: ["functions", projectId] });
    },
  });

  const create = useMutation({
    mutationFn: createFunction,
    onSuccess: () => {
      toast.success("函数创建成功");
      queryClient.invalidateQueries({ queryKey: ["functions", projectId] });
      setCreateOpen(false);
      setName("");
      setRuntime("");
      setTimeoutSeconds("15");
      setSpec("shared-1x");
      setEnabled(true);
    },
  });

  const getSearchText = useCallback(
    (f: FunctionItem) => `${f.id} ${f.name} ${f.runtime}`,
    []
  );

  const handleBulkDelete = async (selected: FunctionItem[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selected.map((f) => deleteFunction(f.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selected.length} 个函数`);
      }
      queryClient.invalidateQueries({ queryKey: ["functions", projectId] });
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  return (
    <>
      <ResourceListPage
        title="Functions"
        description="管理云函数：代码部署、环境变量与执行"
        searchPlaceholder="当前页内搜索函数名称或 ID..."
        isLoading={isLoading}
        items={functions}
        columns={columns}
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
        detailPath={(f) => `/console/functions/${f.id}`}
        toolbarActions={
          writeable ? (
            <Button onClick={() => setCreateOpen(true)}>
              <Plus className="h-4 w-4 mr-2" />
              新建函数
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
            ? (f) => (
                <RowDeleteButton
                  onConfirm={() => remove.mutate(f.id)}
                  loading={remove.isPending}
                />
              )
            : undefined
        }
        emptyTitle="暂无函数"
        emptyDescription="创建函数并上传代码包开始使用"
        emptyAction={
          writeable ? <Button onClick={() => setCreateOpen(true)}>新建函数</Button> : undefined
        }
      />

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>新建函数</DialogTitle>
            <DialogDescription>创建后上传 zip 代码包即可部署</DialogDescription>
          </DialogHeader>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              const timeout = Number.parseInt(timeoutSeconds, 10);
              if (!Number.isFinite(timeout) || timeout < 1 || timeout > 300) {
                toast.error("timeout_seconds 需在 1..300 之间");
                return;
              }
              create.mutate({
                id: crypto.randomUUID(),
                name,
                runtime,
                timeout_seconds: timeout,
                spec,
                enabled,
              });
            }}
          >
            <div className="space-y-2">
              <Label htmlFor="fn-name">名称</Label>
              <Input
                id="fn-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
                placeholder="my-function"
              />
            </div>
            <div className="space-y-2">
              <Label>Runtime</Label>
              <Select value={effectiveRuntime} onValueChange={setRuntime}>
                <SelectTrigger>
                  <SelectValue placeholder="选择运行时" />
                </SelectTrigger>
                <SelectContent>
                  {runtimes.map((r) => (
                    <SelectItem
                      key={r.id}
                      value={r.id}
                      disabled={r.status === "eol"}
                    >
                      {r.name}（{r.id}）
                      {runtimeStatusLabel(r) && ` · ${runtimeStatusLabel(r)}`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                同 runtime ID 内小版本/补丁由平台滚动升级；主版本升级 =
                新 runtime ID（旧 ID 不日落）。eol 项仅供存量函数查看，不可新建。
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="fn-timeout">超时（秒，1..300）</Label>
              <Input
                id="fn-timeout"
                type="number"
                value={timeoutSeconds}
                onChange={(e) => setTimeoutSeconds(e.target.value)}
                min={1}
                max={300}
                required
              />
            </div>
            <div className="space-y-2">
              <Label>规格</Label>
              <Select value={spec} onValueChange={setSpec}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {specifications.map((s) => (
                    <SelectItem key={s.id} value={s.id}>
                      {s.id}（{s.cpu} CPU / {s.memory}）
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <label className="flex items-center gap-2 text-sm">
              <Checkbox
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
              />
              启用函数
            </label>
            <DialogFooter>
              <Button type="submit" disabled={create.isPending}>
                {create.isPending ? "创建中..." : "创建"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </>
  );
}

function executionStatusBadge(status: string) {
  switch (status) {
    case "completed":
      return <Badge variant="secondary">completed</Badge>;
    case "failed":
      return <Badge variant="destructive">failed</Badge>;
    case "running":
    case "building":
      return <Badge variant="outline">{status}</Badge>;
    default:
      return <Badge>{status}</Badge>;
  }
}

function deploymentStatusBadge(status: string) {
  switch (status) {
    case "ready":
      return <Badge variant="secondary">ready</Badge>;
    case "failed":
      return <Badge variant="destructive">failed</Badge>;
    default:
      return <Badge variant="outline">{status}</Badge>;
  }
}

// 源徽章（部署源只读投影）：git@<短SHA> / digest 短码 / zip；title 提示
// 完整 source_url / source_ref（钉死值全貌）。
function deploymentSourceBadge(d: Deployment) {
  switch (d.source_type) {
    case "git": {
      const sha = (d.source_ref ?? "").slice(0, 7);
      return (
        <Badge variant="outline" className="font-mono" title={`${d.source_url ?? ""} @ ${d.source_ref ?? ""}`}>
          git@{sha}
        </Badge>
      );
    }
    case "image": {
      const ref = d.source_ref ?? "";
      const short = ref.startsWith("sha256:")
        ? `sha256:${ref.slice(7, 15)}`
        : ref.slice(0, 15);
      return (
        <Badge variant="outline" className="font-mono" title={`${d.source_url ?? ""} @ ${ref}`}>
          {short}
        </Badge>
      );
    }
    default:
      return <Badge variant="outline">zip</Badge>;
  }
}

function TruncatedNotice({ truncated }: { truncated?: boolean }) {
  if (!truncated) return null;
  return (
    <p className="text-xs text-amber-600">
      ⚠ 输出超过 64KB 已截断
    </p>
  );
}

function ExecutionDialog({
  execution,
  onClose,
}: {
  execution: Execution | null;
  onClose: () => void;
}) {
  return (
    <Dialog open={!!execution} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl max-h-[80vh] overflow-y-auto">
        {execution && (
          <>
            <DialogHeader>
              <DialogTitle className="flex items-center gap-2">
                执行 {execution.id}
                {executionStatusBadge(execution.status)}
              </DialogTitle>
              <DialogDescription>
                deployment {execution.deployment_id} ·{" "}
                {execution.duration_ms}ms · 状态码 {execution.status_code}
                {execution.error && (
                  <span className="block text-destructive mt-1">{execution.error}</span>
                )}
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              <div>
                <Label>stdout</Label>
                <pre className="mt-1 rounded-md bg-muted p-3 text-xs overflow-auto whitespace-pre-wrap break-all">
                  {execution.stdout || "(空)"}
                </pre>
                <TruncatedNotice truncated={execution.stdout_truncated} />
              </div>
              <div>
                <Label>stderr</Label>
                <pre className="mt-1 rounded-md bg-muted p-3 text-xs overflow-auto whitespace-pre-wrap break-all">
                  {execution.stderr || "(空)"}
                </pre>
                <TruncatedNotice truncated={execution.stderr_truncated} />
              </div>
              <div>
                <Label>response</Label>
                <pre className="mt-1 rounded-md bg-muted p-3 text-xs overflow-auto whitespace-pre-wrap break-all">
                  {execution.response || "(空)"}
                </pre>
                <TruncatedNotice truncated={execution.response_truncated} />
              </div>
            </div>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

export function FunctionDetailPage() {
  const { functionId } = useParams<{ functionId: string }>();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const writeable = canWrite(role);
  const platformAdmin = isPlatformAdmin(role);

  const [name, setName] = useState("");
  const [entrypoint, setEntrypoint] = useState("");
  // runtime 可变更（functions-runtime-selection.md §3）：只影响后续新
  // deployment 的构建；eol 项禁选（若存量函数停留在 eol runtime，提示迁移）。
  const [runtime, setRuntime] = useState("");
  const [timeoutSeconds, setTimeoutSeconds] = useState("15");
  const [spec, setSpec] = useState("shared-1x");
  const [enabled, setEnabled] = useState(true);
  const [dataInput, setDataInput] = useState('{"hello":"world"}');
  const [asyncExec, setAsyncExec] = useState(true);
  const [selectedExecution, setSelectedExecution] = useState<Execution | null>(null);
  const [variables, setVariablesState] = useState<Variable[]>([]);
  const [scopesText, setScopesText] = useState("");
  // ——部署源三入口（zip 既有 / git 二期 / image 三期）——
  const [deploySource, setDeploySource] = useState<"zip" | "git" | "image">("zip");
  const [gitUrl, setGitUrl] = useState("");
  const [gitRef, setGitRef] = useState("");
  const [gitDir, setGitDir] = useState("");
  const [gitUsername, setGitUsername] = useState("");
  const [gitToken, setGitToken] = useState("");
  const [imageRefStr, setImageRefStr] = useState("");
  const [imageUsername, setImageUsername] = useState("");
  const [imageToken, setImageToken] = useState("");

  const { data: fn, isLoading } = useQuery({
    queryKey: ["functions", projectId, functionId],
    queryFn: () => getFunction(functionId!),
    enabled: !!functionId,
  });

  const { data: runtimes = [] } = useQuery({
    queryKey: ["functions-runtimes"],
    queryFn: listRuntimes,
  });

  const { data: specifications = [] } = useQuery({
    queryKey: ["functions-specifications"],
    queryFn: listSpecifications,
  });

  // 部署/执行子列表对接服务端分页；执行列表带 status 精确过滤（排障面），
  // 过滤变化 reset 回第一页。
  const deploymentsPaging = useServerPaging();
  const { data: deploymentsData, isLoading: deploymentsLoading } = useQuery({
    queryKey: ["deployments", functionId, deploymentsPaging.pageSize, deploymentsPaging.pageToken],
    queryFn: () =>
      listDeployments(functionId!, {
        pageSize: deploymentsPaging.pageSize,
        pageToken: deploymentsPaging.pageToken,
      }),
    enabled: !!functionId,
    placeholderData: (prev) => prev,
  });
  const deployments = deploymentsData?.rows ?? [];

  const [execStatusFilter, setExecStatusFilter] = useState("");
  const executionsPaging = useServerPaging();
  const { data: executionsData, isLoading: executionsLoading } = useQuery({
    queryKey: [
      "executions",
      functionId,
      execStatusFilter,
      executionsPaging.pageSize,
      executionsPaging.pageToken,
    ],
    queryFn: () =>
      listExecutions(functionId!, {
        pageSize: executionsPaging.pageSize,
        pageToken: executionsPaging.pageToken,
        status: execStatusFilter || undefined,
      }),
    enabled: !!functionId,
    placeholderData: (prev) => prev,
    refetchInterval: 3000,
  });
  const executions = executionsData?.rows ?? [];

  const { data: storedVariables } = useQuery({
    queryKey: ["variables", functionId],
    queryFn: () => getVariables(functionId!),
    enabled: !!functionId,
  });

  useEffect(() => {
    if (!fn) return;
    setName(fn.name);
    setEntrypoint(fn.entrypoint);
    setRuntime(fn.runtime);
    setTimeoutSeconds(String(fn.timeout_seconds));
    setSpec(fn.spec);
    setEnabled(fn.enabled);
    setScopesText((fn.declared_scopes ?? []).join("\n"));
  }, [fn]);

  useEffect(() => {
    if (storedVariables) setVariablesState(storedVariables);
  }, [storedVariables]);

  const update = useMutation({
    mutationFn: (input: {
      name?: string;
      entrypoint?: string;
      runtime?: string;
      timeout_seconds?: number;
      spec?: string;
      enabled?: boolean;
    }) => updateFunction(functionId!, input),
    onSuccess: () => {
      toast.success("函数设置已更新");
      queryClient.invalidateQueries({ queryKey: ["functions", projectId, functionId] });
    },
  });

  const saveVariables = useMutation({
    mutationFn: (vars: Variable[]) => setVariables(functionId!, vars),
    onSuccess: (vars) => {
      toast.success("环境变量已保存");
      // 响应为掩码视图（非空值一律脱敏），回填后仍显示占位符。
      setVariablesState(vars);
    },
  });

  // 执行身份 scopes（P0）：每行一条 "<resource>:<op>"，空 = 无平台访问。
  // 词表校验在服务端（assets/databases/users/groups/storage/subscriptions/
  // payments + read/write）；保存成功后同步输入框与查询缓存。
  const saveScopes = useMutation({
    mutationFn: () => {
      const declaredScopes = scopesText
        .split("\n")
        .map((s) => s.trim())
        .filter((s) => s !== "");
      return setFunctionScopes(functionId!, declaredScopes);
    },
    onSuccess: (fnItem) => {
      toast.success("执行身份 Scopes 已保存");
      setScopesText((fnItem.declared_scopes ?? []).join("\n"));
      queryClient.invalidateQueries({
        queryKey: ["functions", projectId, functionId],
      });
    },
  });

  const upload = useMutation({
    mutationFn: (file: File) => uploadDeployment(functionId!, file),
    onSuccess: () => {
      toast.success("代码包上传成功，正在构建");
      queryClient.invalidateQueries({ queryKey: ["deployments", functionId] });
    },
  });

  // ——git / image 部署源（CreateDeployment JSON 通道）——同步长请求
  // （git 物化+构建 / 镜像导入+强制验证），isPending 覆盖整个过程。
  const deployGit = useMutation({
    mutationFn: () =>
      createDeploymentGit(functionId!, {
        url: gitUrl.trim(),
        ref: gitRef.trim() || undefined,
        directory: gitDir.trim() || undefined,
        username: gitUsername.trim() || undefined,
        token: gitToken || undefined,
      }),
    onSuccess: () => {
      toast.success("git 部署完成（物化 + 构建 + 验证已终态）");
      setGitToken("");
      queryClient.invalidateQueries({ queryKey: ["deployments", functionId] });
    },
  });

  const deployImage = useMutation({
    mutationFn: () =>
      createDeploymentImage(functionId!, {
        image: imageRefStr.trim(),
        registry_username: imageUsername.trim() || undefined,
        registry_token: imageToken || undefined,
      }),
    onSuccess: () => {
      toast.success("镜像导入完成（digest 已钉死，契约验证已通过）");
      setImageToken("");
      queryClient.invalidateQueries({ queryKey: ["deployments", functionId] });
    },
  });

  const removeDeployment = useMutation({
    mutationFn: (deploymentId: string) =>
      deleteDeployment(functionId!, deploymentId),
    onSuccess: () => {
      toast.success("部署已删除");
      queryClient.invalidateQueries({ queryKey: ["deployments", functionId] });
    },
  });

  const run = useMutation({
    mutationFn: () =>
      createExecution(functionId!, { data: dataInput, async: asyncExec }),
    onSuccess: (execution) => {
      toast.success(asyncExec ? "已入队异步执行" : "执行完成");
      queryClient.invalidateQueries({ queryKey: ["executions", functionId] });
      if (!asyncExec) setSelectedExecution(execution);
    },
  });

  const removeFunction = useMutation({
    mutationFn: (id: string) => deleteFunction(id),
    onSuccess: () => {
      toast.success("函数已删除");
      queryClient.invalidateQueries({ queryKey: ["functions", projectId] });
      window.history.back();
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!fn) return <NotFound backTo="/console/functions" />;

  const fnRuntimeInfo = runtimes.find((r) => r.id === fn.runtime);

  const setVariable = (idx: number, key: string, value: string) => {
    setVariablesState((prev) =>
      prev.map((v, i) => (i === idx ? { key, value } : v))
    );
  };

  // isMaskedVariable 判断变量是否处于「已设置，仅设置时可见」的掩码态：
  // 值为 SECRET_MASK 时输入框显示为空 + 占位提示；用户编辑后写入真实值，
  // 保存时未触碰的掩码项仍以 SECRET_MASK 提交，后端保留旧值不覆盖。
  const isMaskedVariable = (v: Variable) => v.value === SECRET_MASK;

  return (
    <div className="space-y-6">
      <DetailPageWrapper
        title={fn.name}
        description={`${fn.runtime} · ${fn.id}`}
        backTo="/console/functions"
        actions={
          writeable ? (
            <DeleteButton
              onConfirm={() => removeFunction.mutate(fn.id)}
              loading={removeFunction.isPending}
            />
          ) : undefined
        }
      >
        <DetailGrid
          items={[
            { label: "ID", value: fn.id, mono: true },
            {
              label: "Runtime",
              value: (
                <span className="flex items-center gap-1.5">
                  {fn.runtime}
                  {runtimeBadge(fnRuntimeInfo)}
                  {fnRuntimeInfo?.status === "eol" && (
                    <span className="text-xs font-normal text-muted-foreground">
                      已停止上游安全补丁，建议迁移到新版本
                    </span>
                  )}
                </span>
              ),
            },
            { label: "Entrypoint", value: fn.entrypoint },
            { label: "创建时间", value: formatDateTime(fn.created_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">基本信息</CardTitle>
        </CardHeader>
        <CardContent>
          <form
            className="grid gap-4 sm:grid-cols-2 max-w-3xl"
            onSubmit={(e) => {
              e.preventDefault();
              const timeout = Number.parseInt(timeoutSeconds, 10);
              if (!Number.isFinite(timeout) || timeout < 1 || timeout > 300) {
                toast.error("timeout_seconds 需在 1..300 之间");
                return;
              }
              update.mutate({
                name,
                entrypoint,
                runtime,
                timeout_seconds: timeout,
                spec,
                enabled,
              });
            }}
          >
            <div className="space-y-2">
              <Label htmlFor="fn-name">名称</Label>
              <Input
                id="fn-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="fn-entrypoint">Entrypoint（MVP 占位，固定 index.js/main.py 的 main）</Label>
              <Input
                id="fn-entrypoint"
                value={entrypoint}
                onChange={(e) => setEntrypoint(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label>Runtime</Label>
              <Select value={runtime} onValueChange={setRuntime}>
                <SelectTrigger>
                  <SelectValue placeholder="选择运行时" />
                </SelectTrigger>
                <SelectContent>
                  {runtimes.map((r) => (
                    <SelectItem
                      key={r.id}
                      value={r.id}
                      disabled={r.status === "eol"}
                    >
                      {r.name}（{r.id}）
                      {runtimeStatusLabel(r) && ` · ${runtimeStatusLabel(r)}`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                变更只影响后续新部署的构建，存量 ready
                部署按其构建时快照继续运行。
                {fnRuntimeInfo?.status === "eol" &&
                  " 当前 runtime 已 EOL（选项已禁用），请迁移到新版本后重新部署。"}
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="fn-timeout">超时（秒，1..300）</Label>
              <Input
                id="fn-timeout"
                type="number"
                value={timeoutSeconds}
                onChange={(e) => setTimeoutSeconds(e.target.value)}
                min={1}
                max={300}
              />
            </div>
            <div className="space-y-2">
              <Label>规格</Label>
              <Select value={spec} onValueChange={setSpec}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {specifications.map((s) => (
                    <SelectItem key={s.id} value={s.id}>
                      {s.id}（{s.cpu} CPU / {s.memory}）
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <label className="flex items-center gap-2 text-sm">
              <Checkbox
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
              />
              启用函数
            </label>
            <div className="sm:col-span-2">
              <Button type="submit" disabled={!writeable || update.isPending}>
                {update.isPending ? "保存中..." : "保存设置"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">执行身份 Scopes</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="space-y-2 max-w-3xl">
            <p className="text-xs text-muted-foreground">
              函数执行时平台注入短期 token（TW_EXECUTION_TOKEN /
              TW_API_BASE_URL），按下方声明访问平台能力；每行一条
              &lt;resource&gt;:&lt;op&gt;（如 assets:write），resource 仅限
              assets / databases / users / groups / storage / subscriptions /
              payments，op 为 read 或 write，留空 = 无平台访问。注意：声明
              databases/storage 后，还需把对应集合/桶的权限授予角色
              <code className="font-mono">key:function:{fn.id}</code>
              ，否则请求通过但读不到数据。
            </p>
            <textarea
              className="flex min-h-[80px] w-full rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50"
              placeholder={"assets:write\ndatabases:read"}
              value={scopesText}
              onChange={(e) => setScopesText(e.target.value)}
              disabled={!writeable}
            />
            <Button
              type="button"
              size="sm"
              disabled={!writeable || saveScopes.isPending}
              onClick={() => saveScopes.mutate()}
            >
              {saveScopes.isPending ? "保存中..." : "保存 Scopes"}
            </Button>
          </div>
        </CardContent>
      </Card>

      <FunctionTriggersCard functionId={functionId!} writeable={writeable} />

      {fn && <FunctionClientPolicyCard fn={fn} writeable={writeable} />}

      {fn && <FunctionPoolPolicyCard fn={fn} writeable={writeable} />}

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">环境变量</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="space-y-2">
            <p className="text-xs text-muted-foreground">
              仅存放第三方服务密钥。平台能力（资产/数据库/存储等）请使用上方
              「执行身份 Scopes」，不要再把平台 API key 存在这里。
            </p>
            {variables.map((v, idx) => (
              <div key={idx} className="flex gap-2">
                <Input
                  className="max-w-[240px] font-mono text-xs"
                  placeholder="KEY"
                  value={v.key}
                  onChange={(e) => setVariable(idx, e.target.value, v.value)}
                />
                <Input
                  className="font-mono text-xs"
                  placeholder={isMaskedVariable(v) ? "已设置，仅设置时可见" : "VALUE"}
                  value={isMaskedVariable(v) ? "" : v.value}
                  onChange={(e) => setVariable(idx, v.key, e.target.value)}
                />
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  onClick={() =>
                    setVariablesState((prev) =>
                      prev.filter((_, i) => i !== idx)
                    )
                  }
                  title="删除变量"
                >
                  <Trash2 className="h-4 w-4 text-destructive" />
                </Button>
              </div>
            ))}
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setVariablesState((prev) => [...prev, { key: "", value: "" }])}
            >
              <Plus className="h-4 w-4 mr-2" />
              添加变量
            </Button>
            <div>
              <Button
                type="button"
                size="sm"
                disabled={!platformAdmin || saveVariables.isPending}
                onClick={() =>
                  saveVariables.mutate(
                    variables.filter((v) => v.key.trim() !== "")
                  )
                }
              >
                {saveVariables.isPending ? "保存中..." : "保存变量"}
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">部署</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-3">
            <div className="flex items-center gap-2">
              <UploadCloud className="h-4 w-4 shrink-0 text-muted-foreground" />
              <Select
                value={deploySource}
                onValueChange={(v) => setDeploySource(v as "zip" | "git" | "image")}
                disabled={!writeable}
              >
                <SelectTrigger className="w-[190px]">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="zip">上传 zip 代码包</SelectItem>
                  <SelectItem value="git">Git 仓库</SelectItem>
                  <SelectItem value="image">镜像引用（BYO）</SelectItem>
                </SelectContent>
              </Select>
              <span className="text-xs text-muted-foreground">
                {deploySource === "zip" && "zip 代码包（≤50MiB，入口 index.js/main.py 的 main）"}
                {deploySource === "git" && "仓库在服务端物化为代码包并钉死 commit"}
                {deploySource === "image" && "契约镜像导入（需符合 Runner 协议，验证强制）"}
              </span>
            </div>

            {deploySource === "zip" && (
              <div className="flex items-center gap-2">
                <Input
                  type="file"
                  accept=".zip"
                  className="max-w-sm"
                  disabled={!writeable}
                  onChange={(e) => {
                    const file = e.target.files?.[0];
                    if (file) {
                      upload.mutate(file);
                      e.target.value = "";
                    }
                  }}
                />
                {upload.isPending && (
                  <span className="text-xs text-muted-foreground">上传中...</span>
                )}
              </div>
            )}

            {deploySource === "git" && (
              <form
                className="space-y-3 max-w-3xl"
                onSubmit={(e) => {
                  e.preventDefault();
                  if (!gitUrl.trim()) {
                    toast.error("仓库地址必填");
                    return;
                  }
                  deployGit.mutate();
                }}
              >
                <div className="grid gap-3 sm:grid-cols-2">
                  <div className="space-y-1.5 sm:col-span-2">
                    <Label htmlFor="deploy-git-url">仓库地址（HTTPS）</Label>
                    <Input
                      id="deploy-git-url"
                      className="font-mono text-xs"
                      placeholder="https://github.com/acme/functions.git"
                      value={gitUrl}
                      onChange={(e) => setGitUrl(e.target.value)}
                      required
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="deploy-git-ref">Ref（分支 / tag / commit，缺省 HEAD）</Label>
                    <Input
                      id="deploy-git-ref"
                      className="font-mono text-xs"
                      placeholder="main"
                      value={gitRef}
                      onChange={(e) => setGitRef(e.target.value)}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="deploy-git-dir">子目录（构建上下文根，缺省仓库根）</Label>
                    <Input
                      id="deploy-git-dir"
                      className="font-mono text-xs"
                      placeholder="functions/greet"
                      value={gitDir}
                      onChange={(e) => setGitDir(e.target.value)}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="deploy-git-user">用户名（私有仓库可选）</Label>
                    <Input
                      id="deploy-git-user"
                      autoComplete="off"
                      value={gitUsername}
                      onChange={(e) => setGitUsername(e.target.value)}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="deploy-git-token">访问 Token（PAT，一次性凭证不落库）</Label>
                    <Input
                      id="deploy-git-token"
                      type="password"
                      autoComplete="new-password"
                      value={gitToken}
                      onChange={(e) => setGitToken(e.target.value)}
                    />
                  </div>
                </div>
                <Button type="submit" size="sm" disabled={!writeable || deployGit.isPending}>
                  <GitBranch className="h-4 w-4 mr-2" />
                  {deployGit.isPending ? "物化并构建中（可能需要数十秒）..." : "从 Git 部署"}
                </Button>
              </form>
            )}

            {deploySource === "image" && (
              <form
                className="space-y-3 max-w-3xl"
                onSubmit={(e) => {
                  e.preventDefault();
                  if (!imageRefStr.trim()) {
                    toast.error("镜像引用必填");
                    return;
                  }
                  deployImage.mutate();
                }}
              >
                <div className="grid gap-3 sm:grid-cols-2">
                  <div className="space-y-1.5 sm:col-span-2">
                    <Label htmlFor="deploy-image-ref">镜像引用（host/repo[:tag|@sha256:...]）</Label>
                    <Input
                      id="deploy-image-ref"
                      className="font-mono text-xs"
                      placeholder="registry.example.com/acme/greet:v1"
                      value={imageRefStr}
                      onChange={(e) => setImageRefStr(e.target.value)}
                      required
                    />
                    <p className="text-xs text-muted-foreground">
                      仅 runtime=image 的函数接受镜像源；tag 在导入期钉死为 digest；
                      基础镜像见 docker/functions-runtime-node。
                    </p>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="deploy-image-user">Registry 用户名（私有镜像可选）</Label>
                    <Input
                      id="deploy-image-user"
                      autoComplete="off"
                      value={imageUsername}
                      onChange={(e) => setImageUsername(e.target.value)}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="deploy-image-token">Registry Token（一次性凭证不落库）</Label>
                    <Input
                      id="deploy-image-token"
                      type="password"
                      autoComplete="new-password"
                      value={imageToken}
                      onChange={(e) => setImageToken(e.target.value)}
                    />
                  </div>
                </div>
                <Button type="submit" size="sm" disabled={!writeable || deployImage.isPending}>
                  <Container className="h-4 w-4 mr-2" />
                  {deployImage.isPending ? "导入并验证中（可能需要数十秒）..." : "从镜像部署"}
                </Button>
              </form>
            )}
          </div>
          {deploymentsLoading ? (
            <p className="text-sm text-muted-foreground">加载中...</p>
          ) : deployments.length === 0 ? (
            <p className="text-sm text-muted-foreground">暂无部署，选择部署源开始</p>
          ) : (
            <div className="divide-y">
              {deployments.map((d: Deployment) => (
                <div key={d.id} className="flex items-center justify-between gap-2 py-2">
                  <div className="min-w-0">
                    <div className="font-mono text-xs truncate">{d.id}</div>
                    <div className="flex items-center gap-2 text-xs text-muted-foreground">
                      {deploymentStatusBadge(d.status)}
                      {deploymentSourceBadge(d)}
                      {d.runtime && (
                        <Badge
                          variant="outline"
                          className="font-mono"
                          title="构建所用 runtime 快照（补构建/审计以此为准）"
                        >
                          {d.runtime}
                        </Badge>
                      )}
                      <span>{formatBytes(d.size)}</span>
                      <span>{formatDateTime(d.created_at, tz)}</span>
                    </div>
                    {d.status === "failed" && d.error && (
                      <p className="text-xs text-destructive break-all">{d.error}</p>
                    )}
                  </div>
                  <RowDeleteButton
                    onConfirm={() => removeDeployment.mutate(d.id)}
                    loading={removeDeployment.isPending}
                    disabled={!writeable}
                  />
                </div>
              ))}
            </div>
          )}
          {!deploymentsLoading && (deployments.length > 0 || deploymentsPaging.hasPrev) && (
            <ListPaginationKeyset
              page={deploymentsPaging.page}
              pageSize={deploymentsPaging.pageSize}
              rowCount={deployments.length}
              hasPrev={deploymentsPaging.hasPrev}
              hasNext={!!deploymentsData?.nextPageToken}
              onPrev={deploymentsPaging.goPrev}
              onNext={() => deploymentsPaging.goNext(deploymentsData?.nextPageToken)}
              onPageSizeChange={deploymentsPaging.setPageSize}
            />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">执行</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="fn-data">Data（JSON，≤64KB）</Label>
            <Input
              id="fn-data"
              className="font-mono text-xs"
              value={dataInput}
              onChange={(e) => setDataInput(e.target.value)}
            />
          </div>
          <div className="flex items-center gap-4">
            <label className="flex items-center gap-2 text-sm">
              <Checkbox
                checked={asyncExec}
                onChange={(e) => setAsyncExec(e.target.checked)}
              />
              异步执行（推荐，规避网关超时）
            </label>
            <Button onClick={() => run.mutate()} disabled={!writeable || run.isPending}>
              <Play className="h-4 w-4 mr-2" />
              {run.isPending ? "执行中..." : asyncExec ? "异步执行" : "同步执行"}
            </Button>
          </div>
          <div className="flex items-center gap-2">
            <Select
              value={execStatusFilter || "all"}
              onValueChange={(v) => {
                setExecStatusFilter(v === "all" ? "" : v);
                executionsPaging.reset();
              }}
            >
              <SelectTrigger className="h-8 w-[150px]">
                <SelectValue placeholder="按状态过滤" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">全部状态</SelectItem>
                <SelectItem value="queued">queued</SelectItem>
                <SelectItem value="building">building</SelectItem>
                <SelectItem value="running">running</SelectItem>
                <SelectItem value="completed">completed</SelectItem>
                <SelectItem value="failed">failed</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {executionsLoading ? (
            <p className="text-sm text-muted-foreground">加载中...</p>
          ) : executions.length === 0 ? (
            <p className="text-sm text-muted-foreground">暂无执行记录</p>
          ) : (
            <div className="divide-y">
              {executions.map((e: Execution) => (
                <button
                  key={e.id}
                  type="button"
                  className="w-full flex items-center justify-between gap-2 py-2 text-left hover:bg-muted rounded-md px-2"
                  onClick={() => setSelectedExecution(e)}
                >
                  <div className="min-w-0">
                    <div className="font-mono text-xs truncate">{e.id}</div>
                    <div className="text-xs text-muted-foreground">
                      {formatDateTime(e.created_at, tz)} · {e.duration_ms}ms
                      {e.error && <span className="text-destructive ml-2">{e.error}</span>}
                    </div>
                  </div>
                  {executionStatusBadge(e.status)}
                </button>
              ))}
            </div>
          )}
          <ListPaginationKeyset
            page={executionsPaging.page}
            pageSize={executionsPaging.pageSize}
            rowCount={executions.length}
            hasPrev={executionsPaging.hasPrev}
            hasNext={!!executionsData?.nextPageToken}
            onPrev={executionsPaging.goPrev}
            onNext={() => executionsPaging.goNext(executionsData?.nextPageToken)}
            onPageSizeChange={executionsPaging.setPageSize}
          />
        </CardContent>
      </Card>

      <ExecutionDialog
        execution={selectedExecution}
        onClose={() => setSelectedExecution(null)}
      />
    </div>
  );
}
