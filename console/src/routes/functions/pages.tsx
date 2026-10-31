import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useCollection, useCollectionData, useCommand, useRecord } from "@lynx-go/uikit-core";
import { toast } from "sonner";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { Plus, Play, KeyRound } from "lucide-react";
import {
  deleteFunction,
  type FunctionItem,
  type Invocation,
} from "@/api/functions";
import {
  functionsResource,
  functionsSearch,
  invocationsResource,
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
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
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
    header: "Name",
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
    header: "Status",
    cell: ({ row }) =>
      row.original.enabled ? (
        <Badge variant="secondary">Enabled</Badge>
      ) : (
        <Badge variant="destructive">Disabled</Badge>
      ),
  },
  {
    id: "created",
    header: "Created",
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
              Fingerprint: <code className="font-mono">{fingerprint}</code>
            </p>
          )}
          <p className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            ⚠ The plaintext secret is shown only this once and cannot be viewed again after
            closing (the admin console only shows the fingerprint). Configure it on the function
            side immediately (FDK environment variables or deployment injection).
          </p>
        </div>
        <DialogFooter>
          <Button onClick={onClose}>I've saved it</Button>
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

  // 行删除命令接线（§12.3）：kit 键（列表 collection + 详情 record）由
  // useCommand 缺省同资源失效自动命中；toast 单源 onSettled。
  const remove = useCommand(removeFunctionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Function deleted");
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
        toast.success("Function created");
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
        toast.error(`Deletion finished: ${succeeded} succeeded, ${failed} failed`);
      } else {
        toast.success(`Deleted ${selectedFunctions.length} functions`);
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
            header: "Actions",
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
        <PageHeader title="Functions" description="Resident service functions: invocation routing, triggers, and webhooks" />

        <KitListPage
          list={shownList}
          toolbar={
            <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Search by name or ID on the current page..."
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
                <Button size="sm" className="ml-auto" onClick={() => setCreateOpen(true)}>
                  <Plus className="h-4 w-4 mr-2" />
                  New Function
                </Button>
              ) : null}
            </div>
          }
          pagination={
            <div className="px-4 pb-4">
              <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
            </div>
          }
          emptyTitle={search !== "" ? "No matching functions" : "No functions yet"}
          emptyDescription={
            search !== ""
              ? "Adjust the search term, or clear it to restore all rows on this page"
              : "Create a function and register a target URL for your resident service to get started"
          }
          emptyAction={
            writeable ? <Button onClick={() => setCreateOpen(true)}>New Function</Button> : undefined
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
            <DialogTitle>New Function</DialogTitle>
            <DialogDescription>
              Register the invocation endpoint (endpoint target) for the function's resident
              service; https is enforced in production
            </DialogDescription>
          </DialogHeader>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              const timeout = Number.parseInt(timeoutSeconds, 10);
              if (!Number.isFinite(timeout) || timeout < 1 || timeout > 300) {
                toast.error("timeout_seconds must be between 1 and 300");
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
              <Label htmlFor="fn-name">Name</Label>
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
                A function is a resident service (runs the FDK on any substrate — fleetly / k8s /
                bare metal). Deployment goes through the fleetly CLI or your own toolchain; the
                platform no longer packages deployments.
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="fn-timeout">Timeout (seconds, 1..300)</Label>
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
              Enable function
            </label>
            <DialogFooter>
              <Button type="submit" disabled={create.pending}>
                {create.pending ? "Creating..." : "Create"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {createdSecret && (
        <OneTimeSecretDialog
          title="Function signing secret"
          description={`Function ${createdSecret.fingerprint ? "created" : "created successfully"}; the platform signs (HMAC-SHA256) requests with this secret when invoking.`}
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
      ⚠ Response exceeded the 1MiB limit and was truncated
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
                Invocation {invocation.id}
                {invocationStatusBadge(invocation.status)}
              </DialogTitle>
              <DialogDescription>
                {invocation.duration_ms}ms · status code {invocation.status_code}
                {invocation.error && (
                  <span className="block text-destructive mt-1">{invocation.error}</span>
                )}
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              <div>
                <Label>response</Label>
                <pre className="mt-1 rounded-md bg-muted p-3 text-xs overflow-auto whitespace-pre-wrap break-all">
                  {invocation.response || "(empty)"}
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

  // 详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧 enabled
  // 同义）；404 → null 空态直出（record 槽方言）；键形 = kit record 键，
  // update/rotate/setScopes 等命令的缺省同资源失效现命中它。
  const functionRecord = useRecord(functionsResource, {
    id: functionId ?? undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const fn = functionRecord.data;
  const isLoading = functionRecord.status === "loading";

  // 调用记录子列表换轨 useCollectionData（子视图 kit 化批）：status 精确过滤
  //（排障面）进 scope 旁路键（全量对象进 queryKey，声明键只定就绪门），变化
  // 回第一页仍走 useServerPaging resetKeys 机制；refetchInterval 3s 现值随迁；
  // 旧 placeholderData = keepPrevious 缺省 true（零代码）。
  const [invStatusFilter, setInvStatusFilter] = useState("");
  const invocationsPaging = useServerPaging(20, { resetKeys: [invStatusFilter] });
  const invocations = useCollectionData(invocationsResource, {
    scope: {
      projectId: projectId ?? undefined,
      functionId: functionId ?? undefined,
      status: invStatusFilter || undefined,
    },
    window: { size: invocationsPaging.pageSize, cursor: invocationsPaging.pageToken || undefined },
    refetchInterval: 3000,
  });

  useEffect(() => {
    if (!fn) return;
    setName(fn.name);
    setTargetUrl(fn.target?.url ?? "");
    setTimeoutSeconds(String(fn.timeout_seconds));
    setEnabled(fn.enabled);
    setScopesText((fn.declared_scopes ?? []).join("\n"));
  }, [fn]);

  // 命令接线（§12.3）：update/rotate/setScopes/remove 挂函数资源——缺省同
  // 资源失效现命中 kit 集合键 + 详情 record 键；invoke 同挂资源，失效由
  // descriptor 显式声明打 invocations 资源键。toast 单源 onSettled。
  const update = useCommand(updateFunctionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Function settings updated");
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
        toast.success("Signing secret rotated (the old secret stays valid for a grace window)");
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
        toast.success("Execution scopes saved");
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
        toast.success(asyncExec ? "Async invocation queued" : "Invocation completed");
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
        toast.success("Function deleted");
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
              label: "Signing secret fingerprint",
              value: (
                <span className="flex items-center gap-2">
                  <code className="font-mono text-xs">
                    {fn.target?.secret_fingerprint || "—"}
                  </code>
                  {fn.target?.secret_rotated_at && (
                    <span className="text-xs text-muted-foreground">
                      Rotated at {formatDateTime(fn.target.secret_rotated_at, tz)}
                    </span>
                  )}
                </span>
              ),
            },
            { label: "Created", value: formatDateTime(fn.created_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">General</CardTitle>
        </CardHeader>
        <CardContent>
          <form
            className="grid gap-4 sm:grid-cols-2 max-w-3xl"
            onSubmit={(e) => {
              e.preventDefault();
              const timeout = Number.parseInt(timeoutSeconds, 10);
              if (!Number.isFinite(timeout) || timeout < 1 || timeout > 300) {
                toast.error("timeout_seconds must be between 1 and 300");
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
              <Label htmlFor="fn-name">Name</Label>
              <Input
                id="fn-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="fn-target-url">Target URL (editing only changes the URL)</Label>
              <Input
                id="fn-target-url"
                className="font-mono text-xs"
                value={targetUrl}
                onChange={(e) => setTargetUrl(e.target.value)}
                placeholder="https://fn.example.com/my-function"
              />
              <p className="text-xs text-muted-foreground">
                Secret changes go through "Rotate signing secret"; pool policy / runtime / resource
                sizing belong to the deployment side (fleetly replicas / k8s HPA / user-managed).
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="fn-timeout">Timeout (seconds, 1..300)</Label>
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
              Enable function
            </label>
            <div className="sm:col-span-2 flex items-center gap-3">
              <Button type="submit" disabled={!writeable || update.pending}>
                {update.pending ? "Saving..." : "Save settings"}
              </Button>
              <Button
                type="button"
                variant="outline"
                disabled={!writeable || rotate.pending}
                onClick={() => rotate.mutate({ id: functionId! })}
              >
                <KeyRound className="h-4 w-4 mr-2" />
                {rotate.pending ? "Rotating..." : "Rotate signing secret"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">Execution scopes</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="space-y-2 max-w-3xl">
            <p className="text-xs text-muted-foreground">
              When the function executes, the platform injects a short-lived token (signed request
              header X-Tw-Execution-Token) that grants access to platform capabilities as declared
              below; one &lt;resource&gt;:&lt;op&gt; per line (e.g. assets:write), where resource is
              limited to
              assets / databases / users / groups / storage / subscriptions /
              payments and op is read or write; empty = no platform access. Note: after declaring
              databases/storage, you must also grant the matching collection/bucket permissions to
              role
              <code className="font-mono">key:function:{fn.id}</code>, otherwise requests pass but
              read no data.
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
              {saveScopes.pending ? "Saving..." : "Save scopes"}
            </Button>
          </div>
        </CardContent>
      </Card>

      <FunctionTriggersCard functionId={functionId!} writeable={writeable} />

      {fn && <FunctionClientPolicyCard fn={fn} writeable={writeable} />}

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">Invocations</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="fn-data">Data (JSON, ≤64KB)</Label>
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
              Async invocation (recommended, avoids gateway timeouts)
            </label>
            <Button
              onClick={() => run.mutate({ id: functionId!, data: dataInput, async: asyncExec })}
              disabled={!writeable || run.pending}
            >
              <Play className="h-4 w-4 mr-2" />
              {run.pending ? "Invoking..." : asyncExec ? "Invoke async" : "Invoke sync"}
            </Button>
          </div>
          <div className="flex items-center gap-2">
            <Select
              value={invStatusFilter || "all"}
              onValueChange={(v) => setInvStatusFilter(v === "all" ? "" : v)}
            >
              <SelectTrigger className="h-8 w-[150px]">
                <SelectValue placeholder="Filter by status" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All statuses</SelectItem>
                <SelectItem value="queued">queued</SelectItem>
                <SelectItem value="running">running</SelectItem>
                <SelectItem value="completed">completed</SelectItem>
                <SelectItem value="failed">failed</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {invocations.status === "loading" ? (
            <p className="text-sm text-muted-foreground">Loading...</p>
          ) : invocations.rows.length === 0 ? (
            <p className="text-sm text-muted-foreground">No invocations yet</p>
          ) : (
            <div className="divide-y">
              {invocations.rows.map((inv: Invocation) => (
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
            rowCount={invocations.rows.length}
            hasPrev={invocationsPaging.hasPrev}
            hasNext={!!invocations.segment?.nextCursor}
            onPrev={invocationsPaging.goPrev}
            onNext={() => invocationsPaging.goNext(invocations.segment?.nextCursor)}
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
          title="New signing secret"
          description="Rotation complete: the new secret takes effect immediately; the old secret stays valid for a grace window (dual-signature transition)."
          secret={rotatedSecret.secret}
          fingerprint={rotatedSecret.fingerprint}
          onClose={() => setRotatedSecret(null)}
        />
      )}
    </div>
  );
}
