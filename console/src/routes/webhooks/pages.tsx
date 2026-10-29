import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/uikit-core";
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
  redeliverDeliveryCommand,
} from "@/kit/webhooks";
import { describeApiError } from "@/kit/errors";
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
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
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
    header: "Status",
    cell: ({ row }) => webhookStatusBadge(row.original.status),
  },
  {
    id: "events",
    header: "Subscriptions",
    cell: ({ row }) => `${row.original.events?.length ?? 0} events`,
  },
  {
    id: "created",
    header: "Created",
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
      <Label htmlFor="wh-events">Subscribed events</Label>
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
        One event per line, using the same eventmatch syntax as Function event triggers:
        document write events
        <code className="font-mono">
          {" "}
          databases.&#123;database&#125;.collections.&#123;collection&#125;.documents.&#123;create|update|delete|*&#125;
        </code>
        (the collection and op segments may be <code className="font-mono">*</code>; the
        database segment must be exact for now); platform behavior events
        <code className="font-mono"> auth.users.* / payments.orders.paid </code>
        etc. (domains = auth/payments/economy/subscriptions; the last segment may be
        <code className="font-mono">*</code>; misspelled domains are rejected outright). 1..64 entries.
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
          <DialogTitle>{title ?? "New signing secret"}</DialogTitle>
          <DialogDescription>
            {description ??
              "Rotation complete: the new secret takes effect immediately and the old one stays valid for a grace window (v2 dual-signature transition; receivers keep working)."}
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <pre className="rounded-md bg-muted p-3 font-mono text-xs break-all whitespace-pre-wrap select-all">
            {secret}
          </pre>
          <p className="text-xs text-muted-foreground">
            Fingerprint: <code className="font-mono">{fingerprint}</code>
          </p>
          <p className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            ⚠ The plaintext is shown only this once and cannot be viewed again after closing.
            Configure it on the receiver now (verify via the X-Tw-Signature header; see the
            Webhooks section of the docs).
          </p>
        </div>
        <DialogFooter>
          <Button onClick={onClose}>I've saved it</Button>
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
          <DialogTitle>{mode === "create" ? "New Webhook" : "Edit Webhook"}</DialogTitle>
          <DialogDescription>
            {mode === "create"
              ? "Subscribe to platform events and deliver them to your HTTPS endpoint (url must be https)"
              : "Unmodified fields are not sent; passing the event list replaces it entirely"}
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            const events = parseEvents(eventsText);
            if (events.length === 0) {
              toast.error("At least one subscribed event is required");
              return;
            }
            onSubmit({ url: url.trim(), events, status });
          }}
        >
          <div className="space-y-2">
            <Label htmlFor="wh-url">Endpoint URL (https)</Label>
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
            <Label>Status</Label>
            <Select value={status} onValueChange={(v) => setStatus(v as "enabled" | "paused")}>
              <SelectTrigger className="w-56">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="enabled">enabled (delivering)</SelectItem>
                <SelectItem value="paused">paused (delivery paused)</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button type="submit" disabled={pending}>
              {pending ? "Saving..." : mode === "create" ? "Create" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function WebhooksListPage() {
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
  // scope/enabled 不再逐处传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用。
  const list = useCollection(webhooksResource, {
    from: "/console/webhooks",
    search: webhooksSearch,
  });

  // 创建命令接线（§12.3）：kit 集合键由 useCommand 自动失效；payload 携带
  // 一次性明文密钥（reveal，关闭即弃——与 functions create 的 target_secret
  // 同语义）。toast 单源 onSettled。
  const create = useCommand(createWebhookCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        setCreatedSecret({ id: result.payload.webhook.id, secret: result.payload.secret });
        toast.success("Webhook created");
        setCreateOpen(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 行删除命令接线：kit 集合键自动失效；存量键由 resource.legacyKeys 桥。
  const remove = useCommand(removeWebhookCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Webhook deleted");
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
            header: "Actions",
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
        <PageHeader title="Webhooks" description="Subscribe to platform events and deliver them to your HTTPS endpoint" />

        <KitListPage
          list={shownList}
          toolbar={
            <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Search URL or event pattern on this page..."
                className="h-8 w-56"
              />
              {writeable ? (
                <Button size="sm" className="ml-auto" onClick={() => setCreateOpen(true)}>
                  <Plus className="h-4 w-4 mr-2" />
                  New Webhook
                </Button>
              ) : null}
            </div>
          }
          pagination={
            <div className="px-4 pb-4">
              <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
            </div>
          }
          emptyTitle={search !== "" ? "No matching Webhooks" : "No Webhooks yet"}
          emptyDescription={
            search !== ""
              ? "Adjust the search term, or clear it to restore all rows on this page"
              : "Create a subscription; platform events will be delivered to your endpoint as CloudEvents envelopes"
          }
          emptyAction={
            writeable ? <Button onClick={() => setCreateOpen(true)}>New Webhook</Button> : undefined
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
        </KitListPage>
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
          title="Signing secret generated"
          description={`Webhook ${createdSecret.id} created; the platform signs deliveries with this secret (HMAC-SHA256) and receivers verify via X-Tw-Signature.`}
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

  // 重投命令接线（§12.3）：资源挂载句柄（expireOld 样板）——存量
  // ["webhook-deliveries"] 键由资源 legacyKeys 桥；toast 单源 onSettled。
  const redeliver = useCommand(redeliverDeliveryCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Reset for immediate redelivery");
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
        <CardTitle className="text-sm">Deliveries</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-xs text-muted-foreground">
          Delivery semantics: 2xx = delivered; non-2xx is retried on a backoff schedule
          (immediate → 10s → 1m → 5m → 30m → 2h → 8h… about 15 attempts capped at 24h).
          Terminal failures (status=failed) are the DLQ and can be redelivered manually
          (a full new delivery round; attempts reset to zero).
        </p>
        <div className="flex items-center gap-2">
          <Select
            value={statusFilter || "all"}
            onValueChange={(v) => setStatusFilter(v === "all" ? "" : v)}
          >
            <SelectTrigger className="h-8 w-[150px]">
              <SelectValue placeholder="Filter by status" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All statuses</SelectItem>
              <SelectItem value="pending">pending</SelectItem>
              <SelectItem value="delivered">delivered</SelectItem>
              <SelectItem value="failed">failed</SelectItem>
            </SelectContent>
          </Select>
        </div>
        {isLoading ? (
          <p className="text-sm text-muted-foreground">Loading...</p>
        ) : deliveries.length === 0 ? (
          <p className="text-sm text-muted-foreground">No deliveries yet</p>
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
  const writeable = canWrite(role);
  const [editOpen, setEditOpen] = useState(false);
  const [rotatedSecret, setRotatedSecret] = useState<{ secret: string; fingerprint: string } | null>(null);

  const { data: hook, isLoading } = useQuery({
    queryKey: ["webhooks", projectId, webhookId],
    queryFn: () => getWebhook(webhookId!),
    enabled: !!webhookId,
  });

  // 命令接线（§12.3）：update/rotate/remove 挂 webhook 资源（kit 集合自动
  // 失效）——存量 ["webhooks"] 详情/列表键由 resource.legacyKeys 桥；字段
  // diff 留在提交处（页面持原实体快照——url/events 仅变更时随请求发送，
  // proto3 optional）。toast 单源 onSettled。
  const update = useCommand(updateWebhookCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Webhook updated");
        setEditOpen(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 轮换 payload 携带一次性明文（reveal，关闭即弃）。
  const rotate = useCommand(rotateWebhookSecretCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Signing secret rotated (old secret stays valid for a grace window)");
        setRotatedSecret({ secret: result.payload.secret, fingerprint: result.payload.secret_fingerprint });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const remove = useCommand(removeWebhookCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Webhook deleted");
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
                Edit
              </Button>
              <Button
                variant="outline"
                disabled={rotate.pending}
                onClick={() => rotate.mutate({ id: webhookId! })}
              >
                <KeyRound className="h-4 w-4 mr-2" />
                {rotate.pending ? "Rotating..." : "Rotate secret"}
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
            { label: "Status", value: webhookStatusBadge(hook.status) },
            {
              label: "Signing secret fingerprint",
              value: <code className="font-mono text-xs">{hook.secret_fingerprint}</code>,
            },
            { label: "Created", value: formatDateTime(hook.created_at, tz) },
            { label: "Updated", value: formatDateTime(hook.updated_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <Card>
        <CardHeader className="space-y-0 pb-3">
          <CardTitle className="text-sm">Subscribed events</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          <div className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <AlertTriangle className="size-4 mt-0.5 shrink-0" />
            <span>
              Delivery is at-least-once with best-effort ordering (strict order is not
              guaranteed); receivers should deduplicate using the envelope's event_id/seq
              and verify the X-Tw-Signature header.
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
