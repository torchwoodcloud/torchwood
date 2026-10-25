import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/ui-kit";
import { toast } from "sonner";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { Plus, KeyRound, RotateCcw, AlertTriangle } from "lucide-react";
import {
  getWebhook,
  listDeliveries,
  type Webhook,
  type WebhookDelivery,
} from "@/api/webhooks";
import {
  webhooksResource,
  webhooksSearch,
  createWebhookCommand,
  updateWebhookCommand,
  removeWebhookCommand,
  rotateWebhookSecretCommand,
  redeliverDeliveryDescriptor,
} from "@/fleet/webhooks";
import { describeApiError } from "@/fleet/errors";
import { PageHeader } from "@/components/PageHeader";
import { ListPaginationKeyset } from "@/components/list/ListToolbar";
import { useServerPaging } from "@/hooks/useServerPaging";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
} from "@/components/resource/shared";
import { DataTable } from "@/components/fleet/query/data-table";
import { KeysetPagination } from "@/components/fleet/query/keyset-pagination";
import { ListPage as FleetListPage } from "@/components/fleet/query/list-page";
import { useAuth } from "@/hooks/useAuth";
import { useUserTimezone } from "@/hooks/useTimezone";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { formatDateTime } from "@/lib/datetime";

// ——Webhooks 一等公民（functions-v4.md §6，D5）——
//
// 订阅 outbox 事件脊柱，按 CloudEvents 1.0 structured mode 投递到 HTTPS
// endpoint。投递语义：2xx = 送达；非 2xx 按重试梯度重投（即时 → 10s → 1m →
// 5m → 30m → 2h → 8h…约 15 次封顶 24h），终态失败行进 DLQ（status=failed），
// Redeliver 手动重投。签名密钥明文仅在轮换响应一次性出现。

function webhookStatusBadge(status: string) {
  return status === "enabled" ? (
    <Badge variant="secondary">enabled</Badge>
  ) : (
    <Badge variant="outline">paused</Badge>
  );
}

function deliveryStatusBadge(status: string) {
  switch (status) {
    case "delivered":
      return <Badge variant="secondary">delivered</Badge>;
    case "failed":
      return <Badge variant="destructive">failed</Badge>;
    default:
      return <Badge variant="outline">pending</Badge>;
  }
}

const webhookColumns = (tz: string): LegacyColumnDef<Webhook, unknown>[] => [
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
    id: "url",
    header: "URL",
    cell: ({ row }) => (
      <span className="inline-block max-w-[260px] truncate font-mono text-xs" title={row.original.url}>
        {row.original.url}
      </span>
    ),
  },
  {
    id: "status",
    header: "状态",
    cell: ({ row }) => webhookStatusBadge(row.original.status),
  },
  {
    id: "events",
    header: "订阅",
    cell: ({ row }) => `${row.original.events?.length ?? 0} 条`,
  },
  {
    id: "created",
    header: "创建时间",
    cell: ({ row }) => formatDateTime(row.original.created_at, tz),
  },
];

// 订阅串编辑（创建/编辑共用）：多行 textarea，一行一条（eventmatch 语法）。
function EventsEditor({
  value,
  onChange,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  return (
    <div className="space-y-1">
      <Label htmlFor="wh-events">订阅事件</Label>
      <textarea
        id="wh-events"
        className="flex min-h-24 w-full rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={
          "databases.app.collections.orders.documents.create\nauth.users.*"
        }
        spellCheck={false}
        disabled={disabled}
      />
      <p className="text-xs text-muted-foreground">
        一行一条，与函数事件触发器同款 eventmatch 语法：文档写事件
        <code className="font-mono">
          {" "}
          databases.&#123;database&#125;.collections.&#123;collection&#125;.documents.&#123;create|update|delete|*&#125;
        </code>
        （collection 与 op 段可为 <code className="font-mono">*</code>，
        database 段一期必须精确）；系统行为事件
        <code className="font-mono"> auth.users.* / payments.orders.paid </code>
        等（域 = auth/payments/economy/subscriptions，尾段可为
        <code className="font-mono">*</code>，拼错域名会被直接拒绝）。1..64 条。
      </p>
    </div>
  );
}

function parseEvents(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}

// OneTimeSecretDialog：创建/轮换密钥的一次性明文回显——仅此一次，关闭后只
// 出指纹。title/description 缺省为轮换文案（创建方传入自定义文案）。
function OneTimeSecretDialog({
  title,
  description,
  secret,
  fingerprint,
  onClose,
}: {
  title?: string;
  description?: string;
  secret: string;
  fingerprint?: string;
  onClose: () => void;
}) {
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>{title ?? "新签名密钥"}</DialogTitle>
          <DialogDescription>
            {description ??
              "轮换完成：新钥即刻生效，旧钥保留一个窗口期（v2 双签过渡，接收方不断流）。"}
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <pre className="rounded-md bg-muted p-3 font-mono text-xs break-all whitespace-pre-wrap select-all">
            {secret}
          </pre>
          <p className="text-xs text-muted-foreground">
            指纹：<code className="font-mono">{fingerprint}</code>
          </p>
          <p className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            ⚠ 明文仅本次显示，关闭后无法再查看。请立即配置到接收方
            （按 X-Tw-Signature 头验签，方案见文档 Webhooks 章节）。
          </p>
        </div>
        <DialogFooter>
          <Button onClick={onClose}>我已保存</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// Create/Edit 共用表单对话框：url + 订阅串 + 启停（编辑时 id 不可变）。
function WebhookFormDialog({
  mode,
  initial,
  open,
  onOpenChange,
  onSubmit,
  pending,
}: {
  mode: "create" | "edit";
  initial?: Webhook;
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSubmit: (input: { url: string; events: string[]; status?: "enabled" | "paused" }) => void;
  pending: boolean;
}) {
  const [url, setUrl] = useState("");
  const [eventsText, setEventsText] = useState("databases.app.collections.*.documents.create");
  const [status, setStatus] = useState<"enabled" | "paused">("enabled");

  useEffect(() => {
    if (!open) return;
    setUrl(initial?.url ?? "");
    setEventsText(initial ? (initial.events ?? []).join("\n") : "databases.app.collections.*.documents.create");
    setStatus((initial?.status as "enabled" | "paused") || "enabled");
  }, [open, initial]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-xl max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{mode === "create" ? "新建 Webhook" : "编辑 Webhook"}</DialogTitle>
          <DialogDescription>
            {mode === "create"
              ? "订阅平台事件并投递到你的 HTTPS endpoint（url 强制 https）"
              : "未修改的字段不随请求发送；订阅串传入 = 全量替换"}
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            const events = parseEvents(eventsText);
            if (events.length === 0) {
              toast.error("至少填写一条订阅事件");
              return;
            }
            onSubmit({ url: url.trim(), events, status });
          }}
        >
          <div className="space-y-2">
            <Label htmlFor="wh-url">Endpoint URL（https）</Label>
            <Input
              id="wh-url"
              className="font-mono text-xs"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              required
              placeholder="https://api.example.com/hooks/torchwood"
            />
          </div>
          <EventsEditor value={eventsText} onChange={setEventsText} />
          <div className="space-y-2">
            <Label>状态</Label>
            <Select value={status} onValueChange={(v) => setStatus(v as "enabled" | "paused")}>
              <SelectTrigger className="w-56">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="enabled">enabled（投递中）</SelectItem>
                <SelectItem value="paused">paused（暂停投递）</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button type="submit" disabled={pending}>
              {pending ? "保存中..." : mode === "create" ? "创建" : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function WebhooksListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const navigate = useNavigate();
  const [createOpen, setCreateOpen] = useState(false);
  const [search, setSearch] = useState("");
  // 创建响应的一次性明文密钥回显（OneTimeSecretDialog）。
  const [createdSecret, setCreatedSecret] = useState<{ id: string; secret: string } | null>(null);
  const writeable = canWrite(role);

  // 列表 fleet 化（§12.9-2）：游标/页大小进 URL（分页栈 = 浏览器历史）；
  // 页内当前页搜索保持本地态（旧页语义）。
  const list = useCollection(webhooksResource, {
    from: "/console/webhooks",
    search: webhooksSearch,
    scope: { projectId: projectId ?? undefined },
    query: { enabled: !!projectId },
  });

  // 创建命令接线（§12.3）：kit 集合键由 useCommand 自动失效；payload 携带
  // 一次性明文密钥（reveal，关闭即弃——与 functions create 的 target_secret
  // 同语义）。toast 单源 onSettled。
  const create = useCommand(createWebhookCommand, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        setCreatedSecret({ id: result.payload.webhook.id, secret: result.payload.secret });
        toast.success("Webhook 已创建");
        setCreateOpen(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 行删除命令接线：kit 集合键自动失效（本页无存量列表键消费方）。
  const remove = useCommand(removeWebhookCommand, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Webhook 已删除");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback(
    (w: Webhook) => `${w.id} ${w.url} ${(w.events ?? []).join(" ")}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((w) => getSearchText(w).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const tableColumns: LegacyColumnDef<Webhook, unknown>[] = [
    ...webhookColumns(tz),
    ...(writeable
      ? [
          {
            id: "actions",
            header: "操作",
            cell: ({ row }: { row: { original: Webhook } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<Webhook, unknown>,
        ]
      : []),
  ];

  return (
    <>
      <div className="space-y-6">
        <PageHeader title="Webhooks" description="订阅平台事件并投递到你的 HTTPS endpoint" />

        <FleetListPage
          list={shownList}
          toolbar={
            <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="当前页内搜索 URL 或订阅串..."
                className="h-8 w-56"
              />
              {writeable ? (
                <Button className="ml-auto" onClick={() => setCreateOpen(true)}>
                  <Plus className="h-4 w-4 mr-2" />
                  新建 Webhook
                </Button>
              ) : null}
            </div>
          }
          pagination={
            <div className="px-4 pb-4">
              <KeysetPagination list={list} />
            </div>
          }
          emptyTitle={search !== "" ? "没有匹配的 Webhook" : "暂无 Webhook"}
          emptyDescription={
            search !== ""
              ? "调整搜索词，或清空恢复当前页全部行"
              : "创建订阅，平台事件将按 CloudEvents 信封投递到你的 endpoint"
          }
          emptyAction={
            writeable ? <Button onClick={() => setCreateOpen(true)}>新建 Webhook</Button> : undefined
          }
        >
          <div className="px-6 pb-2 pt-2">
            <DataTable
              list={shownList}
              columns={tableColumns}
              getRowId={(w) => w.id}
              onRowClick={(w) => navigate({ to: `/console/webhooks/${w.id}` })}
            />
          </div>
        </FleetListPage>
      </div>

      <WebhookFormDialog
        mode="create"
        open={createOpen}
        onOpenChange={setCreateOpen}
        onSubmit={(input) => create.mutate(input)}
        pending={create.pending}
      />

      {createdSecret && (
        <OneTimeSecretDialog
          title="签名密钥已生成"
          description={`Webhook ${createdSecret.id} 创建成功；平台以该密钥签名（HMAC-SHA256）投递请求，接收方按 X-Tw-Signature 验签。`}
          secret={createdSecret.secret}
          onClose={() => setCreatedSecret(null)}
        />
      )}
    </>
  );
}

// 投递记录子视图：DLQ 浏览/重投管理面。status 过滤变化经 resetKeys 自动回
// 第一页（机制，非手动 reset）；行内 Redeliver 仅终态行可重投。
function DeliveriesCard({ webhookId, writeable }: { webhookId: string; writeable: boolean }) {
  const queryClient = useQueryClient();
  const tz = useUserTimezone();
  const [statusFilter, setStatusFilter] = useState("");

  const paging = useServerPaging(20, { resetKeys: [statusFilter] });
  const { data, isLoading } = useQuery({
    queryKey: [
      "webhook-deliveries",
      webhookId,
      statusFilter,
      paging.pageSize,
      paging.pageToken,
    ],
    queryFn: () =>
      listDeliveries(webhookId, {
        pageSize: paging.pageSize,
        pageToken: paging.pageToken,
        status: statusFilter || undefined,
      }),
    placeholderData: (prev) => prev,
    refetchInterval: 5000,
  });
  const deliveries = data?.rows ?? [];

  // 重投命令接线（§12.3，裸 descriptor）：投递行不是 webhooks 集合语义——
  // 存量 ["webhook-deliveries"] 键手动桥；toast 单源 onSettled。
  const redeliver = useCommand(redeliverDeliveryDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("已重置为立即重投");
        void queryClient.invalidateQueries({ queryKey: ["webhook-deliveries", webhookId] });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Card>
      <CardHeader className="space-y-0 pb-3">
        <CardTitle className="text-sm">投递记录</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-xs text-muted-foreground">
          投递语义：2xx = 送达；非 2xx 按重试梯度重投（即时 → 10s → 1m → 5m →
          30m → 2h → 8h…约 15 次封顶 24h）。终态失败（status=failed）即 DLQ，
          可手动 Redeliver（语义 = 全新一轮投递，attempts 归零）。
        </p>
        <div className="flex items-center gap-2">
          <Select
            value={statusFilter || "all"}
            onValueChange={(v) => setStatusFilter(v === "all" ? "" : v)}
          >
            <SelectTrigger className="h-8 w-[150px]">
              <SelectValue placeholder="按状态过滤" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部状态</SelectItem>
              <SelectItem value="pending">pending</SelectItem>
              <SelectItem value="delivered">delivered</SelectItem>
              <SelectItem value="failed">failed</SelectItem>
            </SelectContent>
          </Select>
        </div>
        {isLoading ? (
          <p className="text-sm text-muted-foreground">加载中...</p>
        ) : deliveries.length === 0 ? (
          <p className="text-sm text-muted-foreground">暂无投递记录</p>
        ) : (
          <div className="divide-y">
            {deliveries.map((d: WebhookDelivery) => (
              <div key={d.id} className="flex items-start justify-between gap-2 py-2">
                <div className="min-w-0 space-y-1">
                  <div className="flex items-center gap-2">
                    {deliveryStatusBadge(d.status)}
                    <span className="font-mono text-xs">{d.event_type}</span>
                  </div>
                  <div className="font-mono text-xs text-muted-foreground break-all">
                    {d.event_id}
                    {d.seq > 0 ? ` · seq ${d.seq}` : ""}
                  </div>
                  <div className="text-xs text-muted-foreground">
                    attempts {d.attempts} · last_response{" "}
                    {d.last_response_code > 0 ? d.last_response_code : "—"} ·{" "}
                    {formatDateTime(d.next_attempt_at ?? d.updated_at, tz)}
                    {d.last_error && (
                      <span className="block text-destructive break-all">{d.last_error}</span>
                    )}
                  </div>
                </div>
                {writeable && (
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={redeliver.pending}
                    onClick={() => redeliver.mutate({ webhookId, deliveryId: d.id })}
                  >
                    <RotateCcw className="h-3 w-3 mr-1" />
                    Redeliver
                  </Button>
                )}
              </div>
            ))}
          </div>
        )}
        <ListPaginationKeyset
          page={paging.page}
          pageSize={paging.pageSize}
          rowCount={deliveries.length}
          hasPrev={paging.hasPrev}
          hasNext={!!data?.nextPageToken}
          onPrev={paging.goPrev}
          onNext={() => paging.goNext(data?.nextPageToken)}
          onPageSizeChange={paging.setPageSize}
        />
      </CardContent>
    </Card>
  );
}

export function WebhookDetailPage() {
  const { webhookId } = useParams({ strict: false });
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const writeable = canWrite(role);
  const [editOpen, setEditOpen] = useState(false);
  const [rotatedSecret, setRotatedSecret] = useState<{ secret: string; fingerprint: string } | null>(null);

  const { data: hook, isLoading } = useQuery({
    queryKey: ["webhooks", projectId, webhookId],
    queryFn: () => getWebhook(webhookId!),
    enabled: !!webhookId,
  });

  // 命令接线（§12.3）：update/rotate/remove 挂 webhook 资源（kit 集合自动
  // 失效）+ 存量详情键手动桥；字段 diff 留在提交处（页面持原实体快照——
  // url/events 仅变更时随请求发送，proto3 optional）。toast 单源 onSettled。
  const update = useCommand(updateWebhookCommand, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Webhook 已更新");
        setEditOpen(false);
        void queryClient.invalidateQueries({ queryKey: ["webhooks", projectId, webhookId] });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 轮换 payload 携带一次性明文（reveal，关闭即弃）。
  const rotate = useCommand(rotateWebhookSecretCommand, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("签名密钥已轮换（旧钥保留一个窗口期）");
        setRotatedSecret({ secret: result.payload.secret, fingerprint: result.payload.secret_fingerprint });
        void queryClient.invalidateQueries({ queryKey: ["webhooks", projectId, webhookId] });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const remove = useCommand(removeWebhookCommand, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Webhook 已删除");
        void queryClient.invalidateQueries({ queryKey: ["webhooks", projectId] });
        window.history.back();
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!hook) return <NotFound backTo="/console/webhooks" />;

  return (
    <div className="space-y-6">
      <DetailPageWrapper
        title={hook.id}
        description={hook.url}
        backTo="/console/webhooks"
        actions={
          writeable ? (
            <div className="flex items-center gap-2">
              <Button variant="outline" onClick={() => setEditOpen(true)}>
                编辑
              </Button>
              <Button
                variant="outline"
                disabled={rotate.pending}
                onClick={() => rotate.mutate({ id: webhookId! })}
              >
                <KeyRound className="h-4 w-4 mr-2" />
                {rotate.pending ? "轮换中..." : "轮换密钥"}
              </Button>
              <DeleteButton
                onConfirm={() => remove.mutate({ id: webhookId! })}
                loading={remove.pending}
              />
            </div>
          ) : undefined
        }
      >
        <DetailGrid
          items={[
            { label: "ID", value: hook.id, mono: true },
            { label: "URL", value: hook.url, mono: true },
            { label: "状态", value: webhookStatusBadge(hook.status) },
            {
              label: "签名密钥指纹",
              value: <code className="font-mono text-xs">{hook.secret_fingerprint}</code>,
            },
            { label: "创建时间", value: formatDateTime(hook.created_at, tz) },
            { label: "更新时间", value: formatDateTime(hook.updated_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">订阅事件</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          <div className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <AlertTriangle className="size-4 mt-0.5 shrink-0" />
            <span>
              投递为 at-least-once、尽力保序（不承诺严格序）；接收方以信封的
              event_id/seq 做幂等，并按 X-Tw-Signature 头验签。
            </span>
          </div>
          {(hook.events ?? []).map((ev) => (
            <div key={ev} className="font-mono text-xs break-all">
              {ev}
            </div>
          ))}
        </CardContent>
      </Card>

      <DeliveriesCard webhookId={webhookId!} writeable={writeable} />

      <WebhookFormDialog
        mode="edit"
        initial={hook}
        open={editOpen}
        onOpenChange={setEditOpen}
        onSubmit={(input) => {
          // diff 归提交处（页面持原实体快照）：未修改字段不随请求发送。
          const payload: {
            url?: string;
            events?: string[];
            status?: "enabled" | "paused";
          } = { status: input.status };
          if (input.url !== (hook?.url ?? "")) payload.url = input.url;
          if ((input.events ?? []).join("\n") !== (hook?.events ?? []).join("\n")) {
            payload.events = input.events;
          }
          update.mutate({ id: webhookId!, ...payload });
        }}
        pending={update.pending}
      />

      {rotatedSecret && (
        <OneTimeSecretDialog
          secret={rotatedSecret.secret}
          fingerprint={rotatedSecret.fingerprint}
          onClose={() => setRotatedSecret(null)}
        />
      )}
    </div>
  );
}
