import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/ui-kit";
import { toast } from "sonner";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { Plus, Play, KeyRound } from "lucide-react";
import {
  getFunction,
  deleteFunction,
  listInvocations,
  type FunctionItem,
  type Invocation,
} from "@/api/functions";
import {
  functionsResource,
  functionsSearch,
  createFunctionCommand,
  updateFunctionCommand,
  removeFunctionCommand,
  rotateFunctionSecretCommand,
  setFunctionScopesCommand,
  invokeFunctionCommand,
} from "@/kit/functions";
import { describeApiError } from "@/kit/errors";
import { PageHeader } from "@/components/PageHeader";
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
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS, PAGINATION_ZH } from "@/components/kit/query/pagination-zh";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import { useAuth } from "@/hooks/useAuth";
import { useUserTimezone } from "@/hooks/useTimezone";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { formatDateTime } from "@/lib/datetime";
import { FunctionTriggersCard } from "./triggers-card";
import { FunctionClientPolicyCard } from "./client-policy-card";

const functionColumns = (tz: string): LegacyColumnDef<FunctionItem, unknown>[] => [
  {
    id: "id",
    header: "ID",
    cell: ({ row }) => (
      <span className="inline-block max-w-[140px] truncate font-mono text-xs">
        {row.original.id}
      </span>
    ),
  },
  {
    id: "name",
    header: "名称",
    cell: ({ row }) => row.original.name,
  },
  {
    id: "target",
    header: "Target",
    cell: ({ row }) => (
      <span className="inline-block max-w-[240px] truncate font-mono text-xs" title={row.original.target?.url}>
        {row.original.target?.url ?? "—"}
      </span>
    ),
  },
  {
    id: "enabled",
    header: "状态",
    cell: ({ row }) =>
      row.original.enabled ? (
        <Badge variant="secondary">启用</Badge>
      ) : (
        <Badge variant="destructive">禁用</Badge>
      ),
  },
  {
    id: "created",
    header: "创建时间",
    cell: ({ row }) => formatDateTime(row.original.created_at, tz),
  },
];

// OneTimeSecretDialog：创建/轮换签名密钥的一次性明文回显——仅此一次，
// 关闭后任何面只出指纹。
function OneTimeSecretDialog({
  title,
  description,
  secret,
  fingerprint,
  onClose,
}: {
  title: string;
  description: string;
  secret: string;
  fingerprint?: string;
  onClose: () => void;
}) {
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <pre className="rounded-md bg-muted p-3 font-mono text-xs break-all whitespace-pre-wrap select-all">
            {secret}
          </pre>
          {fingerprint && (
            <p className="text-xs text-muted-foreground">
              指纹：<code className="font-mono">{fingerprint}</code>
            </p>
          )}
          <p className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            ⚠ 明文仅本次显示，关闭后无法再查看（管理面只出指纹）。请立即
            配置到函数侧（FDK 环境变量或部署注入）。
          </p>
        </div>
        <DialogFooter>
          <Button onClick={onClose}>我已保存</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function FunctionsListPage() {
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const navigate = useNavigate();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const [name, setName] = useState("");
  const [targetUrl, setTargetUrl] = useState("");
  const [timeoutSeconds, setTimeoutSeconds] = useState("15");
  const [enabled, setEnabled] = useState(true);
  // 创建响应的一次性 target 签名密钥回显。
  const [createdSecret, setCreatedSecret] = useState<{ secret: string; fingerprint: string } | null>(null);
  const writeable = canWrite(role);

  // 列表 fleet 化（§12.9-2）：游标/页大小进 URL（分页栈 = 浏览器历史）；
  // 页内当前页搜索保持本地态（旧页语义）。
  // scope/enabled 不再逐处传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用。
  const list = useCollection(functionsResource, {
    from: "/console/functions",
    search: functionsSearch,
  });

  // 行删除命令接线（§12.3）：kit 集合键由 useCommand 自动失效；存量键由
  // resource.legacyKeys 桥；toast 单源 onSettled。
  const remove = useCommand(removeFunctionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("函数已删除");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 创建命令：payload 携带一次性 target_secret（reveal，关闭即弃）。
  const create = useCommand(createFunctionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("函数创建成功");
        setCreateOpen(false);
        setName("");
        setTargetUrl("");
        setTimeoutSeconds("15");
        setEnabled(true);
        setCreatedSecret({
          secret: result.payload.target_secret,
          fingerprint: result.payload.function.target?.secret_fingerprint ?? "",
        });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback(
    (f: FunctionItem) => `${f.id} ${f.name} ${f.target?.url ?? ""}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((f) => getSearchText(f).includes(search.toLowerCase()));
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
  const selectedFunctions = list.rows.filter((f) => selectedIds.has(f.id));

  const handleBulkDelete = async () => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selectedFunctions.map((f) => deleteFunction(f.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selectedFunctions.length} 个函数`);
      }
      list.refetch();
      setSelectedIds(new Set());
    } finally {
      setBulkDeleting(false);
    }
  };

  const tableColumns: LegacyColumnDef<FunctionItem, unknown>[] = [
    ...functionColumns(tz),
    ...(writeable
      ? [
          {
            id: "actions",
            header: "操作",
            cell: ({ row }: { row: { original: FunctionItem } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<FunctionItem, unknown>,
        ]
      : []),
  ];

  return (
    <>
      <div className="space-y-6">
        <PageHeader title="Functions" description="常驻服务函数：调用路由、触发器与 Webhooks" />

        <KitListPage
          list={shownList}
          toolbar={
            <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="当前页内搜索函数名称或 ID..."
                className="h-8 w-56"
              />
              {selectedFunctions.length > 0 && writeable ? (
                <BulkDeleteButton
                  count={selectedFunctions.length}
                  loading={bulkDeleting}
                  onConfirm={() => void handleBulkDelete()}
                />
              ) : null}
              {writeable ? (
                <Button className="ml-auto" onClick={() => setCreateOpen(true)}>
                  <Plus className="h-4 w-4 mr-2" />
                  新建函数
                </Button>
              ) : null}
            </div>
          }
          pagination={
            <div className="px-4 pb-4">
              <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} labels={PAGINATION_ZH} />
            </div>
          }
          emptyTitle={search !== "" ? "没有匹配的函数" : "暂无函数"}
          emptyDescription={
            search !== ""
              ? "调整搜索词，或清空恢复当前页全部行"
              : "创建函数并登记常驻服务的 target URL 开始使用"
          }
          emptyAction={
            writeable ? <Button onClick={() => setCreateOpen(true)}>新建函数</Button> : undefined
          }
        >
          <div className="px-6 pb-2 pt-2">
            <DataTable
              list={shownList}
              columns={tableColumns}
              getRowId={(f) => f.id}
              onRowClick={(f) => navigate({ to: `/console/functions/${f.id}` })}
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

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>新建函数</DialogTitle>
            <DialogDescription>
              登记函数常驻服务的调用终点（endpoint target；生产强制 https）
            </DialogDescription>
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
                timeout_seconds: timeout,
                enabled,
                target_url: targetUrl.trim(),
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
              <Label htmlFor="fn-target-url">Target URL</Label>
              <Input
                id="fn-target-url"
                className="font-mono text-xs"
                value={targetUrl}
                onChange={(e) => setTargetUrl(e.target.value)}
                required
                placeholder="https://fn.example.com/my-function"
              />
              <p className="text-xs text-muted-foreground">
                函数 = 常驻服务（跑 FDK，任意 substrate——fleetly / k8s / 裸机）。
                部署链走 fleetly CLI 或用户自有工具链，平台不再打包部署。
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
            <label className="flex items-center gap-2 text-sm">
              <Checkbox
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
              />
              启用函数
            </label>
            <DialogFooter>
              <Button type="submit" disabled={create.pending}>
                {create.pending ? "创建中..." : "创建"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {createdSecret && (
        <OneTimeSecretDialog
          title="函数签名密钥"
          description={`函数 ${createdSecret.fingerprint ? "已创建" : "创建成功"}；调用时平台以该密钥签名（HMAC-SHA256）请求。`}
          secret={createdSecret.secret}
          fingerprint={createdSecret.fingerprint}
          onClose={() => setCreatedSecret(null)}
        />
      )}
    </>
  );
}

// v4 收敛：状态集 queued/running/completed/failed（building 态随补构建退役）。
function invocationStatusBadge(status: string) {
  switch (status) {
    case "completed":
      return <Badge variant="secondary">completed</Badge>;
    case "failed":
      return <Badge variant="destructive">failed</Badge>;
    case "running":
      return <Badge variant="outline">running</Badge>;
    default:
      return <Badge>{status}</Badge>;
  }
}

function TruncatedNotice({ truncated }: { truncated?: boolean }) {
  if (!truncated) return null;
  return (
    <p className="text-xs text-amber-600">
      ⚠ 响应超过 1MiB 上限已截断
    </p>
  );
}

function InvocationDialog({
  invocation,
  onClose,
}: {
  invocation: Invocation | null;
  onClose: () => void;
}) {
  return (
    <Dialog open={!!invocation} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl max-h-[80vh] overflow-y-auto">
        {invocation && (
          <>
            <DialogHeader>
              <DialogTitle className="flex items-center gap-2">
                调用 {invocation.id}
                {invocationStatusBadge(invocation.status)}
              </DialogTitle>
              <DialogDescription>
                {invocation.duration_ms}ms · 状态码 {invocation.status_code}
                {invocation.error && (
                  <span className="block text-destructive mt-1">{invocation.error}</span>
                )}
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              <div>
                <Label>response</Label>
                <pre className="mt-1 rounded-md bg-muted p-3 text-xs overflow-auto whitespace-pre-wrap break-all">
                  {invocation.response || "(空)"}
                </pre>
                <TruncatedNotice truncated={invocation.response_truncated} />
              </div>
            </div>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

export function FunctionDetailPage() {
  const { functionId } = useParams({ strict: false });
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const writeable = canWrite(role);

  const [name, setName] = useState("");
  const [targetUrl, setTargetUrl] = useState("");
  const [timeoutSeconds, setTimeoutSeconds] = useState("15");
  const [enabled, setEnabled] = useState(true);
  const [dataInput, setDataInput] = useState('{"hello":"world"}');
  const [asyncExec, setAsyncExec] = useState(true);
  const [selectedInvocation, setSelectedInvocation] = useState<Invocation | null>(null);
  const [scopesText, setScopesText] = useState("");
  // 轮换响应的一次性明文回显。
  const [rotatedSecret, setRotatedSecret] = useState<{ secret: string; fingerprint: string } | null>(null);

  const { data: fn, isLoading } = useQuery({
    queryKey: ["functions", projectId, functionId],
    queryFn: () => getFunction(functionId!),
    enabled: !!functionId,
  });

  // 调用记录子列表对接服务端分页；status 精确过滤（排障面）变化经 resetKeys
  // 自动回第一页（机制，非手动 reset）。
  const [invStatusFilter, setInvStatusFilter] = useState("");
  const invocationsPaging = useServerPaging(20, { resetKeys: [invStatusFilter] });
  const { data: invocationsData, isLoading: invocationsLoading } = useQuery({
    queryKey: [
      "invocations",
      functionId,
      invStatusFilter,
      invocationsPaging.pageSize,
      invocationsPaging.pageToken,
    ],
    queryFn: () =>
      listInvocations(functionId!, {
        pageSize: invocationsPaging.pageSize,
        pageToken: invocationsPaging.pageToken,
        status: invStatusFilter || undefined,
      }),
    enabled: !!functionId,
    placeholderData: (prev) => prev,
    refetchInterval: 3000,
  });
  const invocations = invocationsData?.rows ?? [];

  useEffect(() => {
    if (!fn) return;
    setName(fn.name);
    setTargetUrl(fn.target?.url ?? "");
    setTimeoutSeconds(String(fn.timeout_seconds));
    setEnabled(fn.enabled);
    setScopesText((fn.declared_scopes ?? []).join("\n"));
  }, [fn]);

  // 命令接线（§12.3）：update/rotate/setScopes/remove 挂函数资源（kit 集合
  // 自动失效）——存量 ["functions"] 详情/列表键由 resource.legacyKeys 桥；
  // invoke 用资源挂载句柄（expireOld 样板）——存量 ["invocations"] 键同经
  // 资源 legacyKeys 桥。toast 单源 onSettled。
  const update = useCommand(updateFunctionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("函数设置已更新");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 轮换 payload 携带一次性明文（reveal，关闭即弃）。
  const rotate = useCommand(rotateFunctionSecretCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("签名密钥已轮换（旧钥保留一个宽限窗）");
        setRotatedSecret({ secret: result.payload.secret, fingerprint: result.payload.secret_fingerprint });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 执行身份 scopes（P0）：每行一条 "<resource>:<op>"，空 = 无平台访问。
  // 词表校验在服务端（assets/databases/users/groups/storage/subscriptions/
  // payments + read/write）；多行文本在 descriptor run 内收敛；保存成功后
  // 同步输入框。
  const saveScopes = useCommand(setFunctionScopesCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("执行身份 Scopes 已保存");
        setScopesText((result.payload.declared_scopes ?? []).join("\n"));
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const run = useCommand(invokeFunctionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success(asyncExec ? "已入队异步调用" : "调用完成");
        if (!asyncExec) setSelectedInvocation(result.payload);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const removeFunction = useCommand(removeFunctionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("函数已删除");
        window.history.back();
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!fn) return <NotFound backTo="/console/functions" />;

  return (
    <div className="space-y-6">
      <DetailPageWrapper
        title={fn.name}
        description={`${fn.target?.url ?? ""} · ${fn.id}`}
        backTo="/console/functions"
        actions={
          writeable ? (
            <DeleteButton
              onConfirm={() => removeFunction.mutate({ id: fn.id })}
              loading={removeFunction.pending}
            />
          ) : undefined
        }
      >
        <DetailGrid
          items={[
            { label: "ID", value: fn.id, mono: true },
            { label: "Target URL", value: fn.target?.url ?? "—", mono: true },
            {
              label: "签名密钥指纹",
              value: (
                <span className="flex items-center gap-2">
                  <code className="font-mono text-xs">
                    {fn.target?.secret_fingerprint || "—"}
                  </code>
                  {fn.target?.secret_rotated_at && (
                    <span className="text-xs text-muted-foreground">
                      轮换于 {formatDateTime(fn.target.secret_rotated_at, tz)}
                    </span>
                  )}
                </span>
              ),
            },
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
              const input: {
                name?: string;
                timeout_seconds?: number;
                enabled?: boolean;
                target_url?: string;
              } = {
                name,
                timeout_seconds: timeout,
                enabled,
              };
              // target url 仅在变更时随请求发送（proto3 optional——未设置 = 不修改）。
              if (targetUrl.trim() !== (fn.target?.url ?? "")) {
                input.target_url = targetUrl.trim();
              }
              update.mutate({ id: functionId!, ...input });
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
              <Label htmlFor="fn-target-url">Target URL（编辑仅改地址）</Label>
              <Input
                id="fn-target-url"
                className="font-mono text-xs"
                value={targetUrl}
                onChange={(e) => setTargetUrl(e.target.value)}
                placeholder="https://fn.example.com/my-function"
              />
              <p className="text-xs text-muted-foreground">
                密钥变更走「轮换密钥」；池策略/运行时/资源规格归部署侧
                （fleetly 副本数 / k8s HPA / 用户自管）。
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
            <label className="flex items-center gap-2 text-sm">
              <Checkbox
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
              />
              启用函数
            </label>
            <div className="sm:col-span-2 flex items-center gap-3">
              <Button type="submit" disabled={!writeable || update.pending}>
                {update.pending ? "保存中..." : "保存设置"}
              </Button>
              <Button
                type="button"
                variant="outline"
                disabled={!writeable || rotate.pending}
                onClick={() => rotate.mutate({ id: functionId! })}
              >
                <KeyRound className="h-4 w-4 mr-2" />
                {rotate.pending ? "轮换中..." : "轮换签名密钥"}
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
              函数执行时平台注入短期 token（签名请求头 X-Tw-Execution-Token），
              按下方声明访问平台能力；每行一条 &lt;resource&gt;:&lt;op&gt;
              （如 assets:write），resource 仅限
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
              disabled={!writeable || saveScopes.pending}
              onClick={() => saveScopes.mutate({ id: functionId!, scopesText })}
            >
              {saveScopes.pending ? "保存中..." : "保存 Scopes"}
            </Button>
          </div>
        </CardContent>
      </Card>

      <FunctionTriggersCard functionId={functionId!} writeable={writeable} />

      {fn && <FunctionClientPolicyCard fn={fn} writeable={writeable} />}

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">调用（Invocations）</CardTitle>
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
              异步调用（推荐，规避网关超时）
            </label>
            <Button
              onClick={() => run.mutate({ id: functionId!, data: dataInput, async: asyncExec })}
              disabled={!writeable || run.pending}
            >
              <Play className="h-4 w-4 mr-2" />
              {run.pending ? "调用中..." : asyncExec ? "异步调用" : "同步调用"}
            </Button>
          </div>
          <div className="flex items-center gap-2">
            <Select
              value={invStatusFilter || "all"}
              onValueChange={(v) => setInvStatusFilter(v === "all" ? "" : v)}
            >
              <SelectTrigger className="h-8 w-[150px]">
                <SelectValue placeholder="按状态过滤" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">全部状态</SelectItem>
                <SelectItem value="queued">queued</SelectItem>
                <SelectItem value="running">running</SelectItem>
                <SelectItem value="completed">completed</SelectItem>
                <SelectItem value="failed">failed</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {invocationsLoading ? (
            <p className="text-sm text-muted-foreground">加载中...</p>
          ) : invocations.length === 0 ? (
            <p className="text-sm text-muted-foreground">暂无调用记录</p>
          ) : (
            <div className="divide-y">
              {invocations.map((inv: Invocation) => (
                <button
                  key={inv.id}
                  type="button"
                  className="w-full flex items-center justify-between gap-2 py-2 text-left hover:bg-muted rounded-md px-2"
                  onClick={() => setSelectedInvocation(inv)}
                >
                  <div className="min-w-0">
                    <div className="font-mono text-xs truncate">{inv.id}</div>
                    <div className="text-xs text-muted-foreground">
                      {formatDateTime(inv.created_at, tz)} · {inv.duration_ms}ms
                      {inv.error && <span className="text-destructive ml-2">{inv.error}</span>}
                    </div>
                  </div>
                  {invocationStatusBadge(inv.status)}
                </button>
              ))}
            </div>
          )}
          <ListPaginationKeyset
            page={invocationsPaging.page}
            pageSize={invocationsPaging.pageSize}
            rowCount={invocations.length}
            hasPrev={invocationsPaging.hasPrev}
            hasNext={!!invocationsData?.nextPageToken}
            onPrev={invocationsPaging.goPrev}
            onNext={() => invocationsPaging.goNext(invocationsData?.nextPageToken)}
            onPageSizeChange={invocationsPaging.setPageSize}
          />
        </CardContent>
      </Card>

      <InvocationDialog
        invocation={selectedInvocation}
        onClose={() => setSelectedInvocation(null)}
      />

      {rotatedSecret && (
        <OneTimeSecretDialog
          title="新签名密钥"
          description="轮换完成：新钥即刻生效，旧钥保留一个宽限窗（双签过渡）。"
          secret={rotatedSecret.secret}
          fingerprint={rotatedSecret.fingerprint}
          onClose={() => setRotatedSecret(null)}
        />
      )}
    </div>
  );
}
