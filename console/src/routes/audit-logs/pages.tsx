import { useState } from "react";
import { useCollection } from "@lynx-go/ui-kit";
import { predicateOf } from "@lynx-go/ui-kit/schema";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import type { Predicate } from "@lynx-go/ui-kit/kernel";
import type { AuditLog } from "@/api/auditLogs";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue, toDateTimeLocalValue } from "@/lib/datetime";
import { PageHeader } from "@/components/PageHeader";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { JsonEditor } from "@/components/ui/json-editor";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { DataTable } from "@/components/fleet/query/data-table";
import { KeysetPagination } from "@/components/fleet/query/keyset-pagination";
import { ListPage as FleetListPage } from "@/components/fleet/query/list-page";
import {
  auditLogsResource,
  auditLogsSearch,
  filtersOf,
  type FilterField,
} from "@/fleet/auditLogs";

// —— 语义化渲染辅助：把 gRPC FullMethod / 结构化 metadata 变成人话 ——

const VERB_LABELS: Record<string, string> = {
  Create: "创建",
  Update: "更新",
  Delete: "删除",
  Deploy: "部署",
  Set: "设置",
  Replay: "重放",
  Rotate: "轮换",
  Revoke: "吊销",
  Ban: "封禁",
  Enable: "启用",
  Disable: "停用",
  SignIn: "登录",
  SignOut: "登出",
  Refresh: "刷新",
  Upload: "上传",
  Import: "导入",
  Sync: "同步",
};

const CHANNEL_LABELS: Record<string, string> = {
  cli: "CLI",
  sdk: "SDK",
  console: "Console",
  function: "Function",
  api: "API Key",
  user: "用户",
};

interface ActionParts {
  service: string; // 如 FunctionsService
  method: string; // 如 UpdateFunction
  verb: string; // 如 Update
}

function parseAction(action: string): ActionParts {
  // 形如 /torchwood.server.v1.FunctionsService/UpdateFunction
  const rest = action.startsWith("/") ? action.slice(1) : action;
  const [svc = "", method = ""] = rest.split("/");
  const m = method.match(/^([A-Z][a-z]+)/);
  return { service: svc.split(".").pop() ?? svc, method, verb: m?.[1] ?? method };
}

function verbLabel(verb: string): string {
  return VERB_LABELS[verb] ?? verb;
}

function channelOf(log: AuditLog): string {
  const client = log.metadata?.["client"] as
    | { channel?: string }
    | undefined;
  return client?.channel ?? "";
}

function statusBadgeVariant(status: string): "default" | "destructive" | "secondary" {
  if (status === "success") return "default";
  if (status === "denied" || status === "error" || status === "throttled") return "destructive";
  // gRPC code 名（permission_denied 等）
  return "secondary";
}

// describeAudit 生成语义化一行：如「CLI 更新了 Functions（client_per_user_limit: 10 → 20）」。
function describeAudit(log: AuditLog): string {
  const parts = parseAction(log.action);
  const channel = channelOf(log);
  const channelLabel = channel ? `${CHANNEL_LABELS[channel] ?? channel} ` : "";
  const changes = log.metadata?.["changes"] as
    | Record<string, { from?: unknown; to?: unknown }>
    | undefined;
  let detail = "";
  if (changes) {
    const fields = Object.keys(changes);
    if (fields.length > 0) {
      detail = `（${fields.map((f) => humanizeChange(f, changes[f])).join("，")}）`;
    }
  }
  return `${channelLabel}${verbLabel(parts.verb)} ${parts.method.slice(parts.verb.length) || parts.service}${detail}`;
}

function humanizeChange(field: string, change: { from?: unknown; to?: unknown }): string {
  return `${field}: ${formatValue(change.from)} → ${formatValue(change.to)}`;
}

function formatValue(v: unknown): string {
  if (v === undefined || v === null) return "—";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

// —— 页面 ——

export function AuditLogsListPage() {
  const { role } = useAdminRole();
  const platformAdmin = isPlatformAdmin(role);
  const tz = useUserTimezone();

  // keyset 切片：过滤/排序/游标/页大小全部进 URL（filter=field:op:value /
  // sort / cursor / size），游标分页栈 = 浏览器历史。from 用路由 id（代码式
  // 路由树中 id = 完整路径；useSearch 按 id 锚定活跃 match）。
  const list = useCollection(auditLogsResource, {
    from: "/console/audit-logs",
    search: auditLogsSearch,

  });
  const filters = filtersOf(list.query.predicate);
  const [selected, setSelected] = useState<AuditLog | null>(null);

  // setFilter 写谓词（replace + 清游标回第一页）；datetime 字段在写入边界
  // 做墙钟↔绝对换算——URL 存绝对时刻，跨时区分享同一过滤窗口。叶子归一化
  // 用 kit predicateOf（0→null / 1→裸叶 / n→AND，§12.10-④）。
  const setFilter = (field: FilterField, value: string) => {
    const next = { ...filters, [field]: value || undefined };
    const leaves: Predicate[] = [];
    for (const [f, v] of Object.entries(next)) {
      if (f === "created_after" || f === "created_before") continue;
      if (typeof v === "string" && v !== "") leaves.push({ field: f, op: "eq", value: v });
    }
    const after = fromDateTimeLocalValue(next.created_after ?? "", tz);
    if (after) leaves.push({ field: "created_after", op: "eq", value: after });
    const before = fromDateTimeLocalValue(next.created_before ?? "", tz);
    if (before) leaves.push({ field: "created_before", op: "eq", value: before });
    list.setPredicate(predicateOf(leaves));
  };

  const columns: LegacyColumnDef<AuditLog, unknown>[] = [
    {
      id: "created_at",
      header: "时间",
      meta: { sortField: "created_at" },
      cell: ({ row }) => (
        <span className="whitespace-nowrap text-xs text-muted-foreground">
          {row.original.created_at ? formatDateTime(row.original.created_at, tz) : "—"}
        </span>
      ),
    },
    {
      id: "change",
      header: "变更",
      cell: ({ row }) => (
        <div className="max-w-[320px]">
          <div className="truncate font-medium" title={describeAudit(row.original)}>
            {describeAudit(row.original)}
          </div>
          <div
            className="truncate font-mono text-xs text-muted-foreground"
            title={row.original.action}
          >
            {row.original.action}
          </div>
        </div>
      ),
    },
    {
      id: "status",
      header: "状态",
      cell: ({ row }) => (
        <Badge variant={statusBadgeVariant(row.original.status)}>{row.original.status}</Badge>
      ),
    },
    {
      id: "channel",
      header: "通道",
      cell: ({ row }) => {
        const channel = channelOf(row.original);
        return channel ? (
          <Badge variant="outline">{CHANNEL_LABELS[channel] ?? channel}</Badge>
        ) : (
          <span className="text-muted-foreground">—</span>
        );
      },
    },
    {
      id: "actor",
      header: "操作者",
      cell: ({ row }) => (
        <span className="block max-w-[160px] truncate font-mono text-xs">
          {row.original.actor_kind ? `${row.original.actor_kind}:` : ""}
          {row.original.actor_id || "—"}
        </span>
      ),
    },
    {
      id: "resource",
      header: "资源",
      cell: ({ row }) => (
        <span className="block max-w-[140px] truncate font-mono text-xs">
          {row.original.resource_id || "—"}
        </span>
      ),
    },
    {
      id: "ip",
      header: "IP",
      cell: ({ row }) => (
        <span className="font-mono text-xs">{row.original.ip || "—"}</span>
      ),
    },
  ];

  // 源空与过滤空两套文案：谓词在场 = 过滤空。
  const filtered = list.query.predicate != null;

  return (
    <div className="space-y-6">
      <PageHeader
        title="审计日志"
        description="管理面（Console / CLI / SDK / API Key）全部系统变更的完整审计轨迹"
      />

      <FleetListPage
        list={list}
        toolbar={
          <div className="space-y-4 p-6 pb-0">
            <div className="grid grid-cols-1 gap-3 md:grid-cols-3 xl:grid-cols-6">
              <div className="space-y-1">
                <Label htmlFor="audit-action">操作</Label>
                <Input
                  id="audit-action"
                  placeholder="如 FunctionsService/UpdateFunction"
                  value={filters.action ?? ""}
                  onChange={(e) => setFilter("action", e.target.value)}
                />
              </div>
              <div className="space-y-1">
                <Label>状态</Label>
                <Select
                  value={filters.status ?? "all"}
                  onValueChange={(v) => setFilter("status", v === "all" ? "" : v)}
                >
                  <SelectTrigger aria-label="状态">
                    <SelectValue placeholder="全部" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">全部</SelectItem>
                    <SelectItem value="success">success</SelectItem>
                    <SelectItem value="denied">denied</SelectItem>
                    <SelectItem value="throttled">throttled</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="audit-actor">操作者 ID</Label>
                <Input
                  id="audit-actor"
                  value={filters.actor_id ?? ""}
                  onChange={(e) => setFilter("actor_id", e.target.value)}
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="audit-resource">资源 ID</Label>
                <Input
                  id="audit-resource"
                  value={filters.resource_id ?? ""}
                  onChange={(e) => setFilter("resource_id", e.target.value)}
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="audit-after">起始时间</Label>
                <Input
                  id="audit-after"
                  type="datetime-local"
                  value={toDateTimeLocalValue(filters.created_after, tz)}
                  onChange={(e) => setFilter("created_after", e.target.value)}
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="audit-before">截止时间</Label>
                <Input
                  id="audit-before"
                  type="datetime-local"
                  value={toDateTimeLocalValue(filters.created_before, tz)}
                  onChange={(e) => setFilter("created_before", e.target.value)}
                />
              </div>
            </div>
            {platformAdmin && (
              <div className="flex flex-wrap items-center gap-3">
                <div className="w-56 space-y-1">
                  <Label>视图范围</Label>
                  <Select
                    value={filters.scope ?? "project"}
                    onValueChange={(v) => setFilter("scope", v === "project" ? "" : v)}
                  >
                    <SelectTrigger aria-label="视图范围">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="project">当前项目</SelectItem>
                      <SelectItem value="include_platform">当前项目 + 平台级</SelectItem>
                      <SelectItem value="all_projects">全部项目</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
              </div>
            )}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} />
          </div>
        }
        emptyTitle={filtered ? "没有匹配的审计记录" : "暂无审计记录"}
        emptyDescription={
          filtered
            ? "调整过滤条件，或清空过滤恢复全部记录"
            : "等待系统变更产生新的审计行"
        }
        appendix={
          <Dialog open={!!selected} onOpenChange={(open) => !open && setSelected(null)}>
            <DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
              <DialogHeader>
                <DialogTitle className="font-mono text-base">{selected?.action}</DialogTitle>
              </DialogHeader>
              {selected && <AuditLogDetail log={selected} />}
            </DialogContent>
          </Dialog>
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={list}
            columns={columns}
            getRowId={(row) => row.id}
            onRowClick={(log) => setSelected(log)}
          />
        </div>
      </FleetListPage>
    </div>
  );
}

// AuditLogDetail 渲染详情：全字段 + 结构化变更内容分区（changes / request / client）。
function AuditLogDetail({ log }: { log: AuditLog }) {
  const tz = useUserTimezone();
  const changes = log.metadata?.["changes"] as
    | Record<string, { from?: unknown; to?: unknown }>
    | undefined;
  const request = log.metadata?.["request"];
  const client = log.metadata?.["client"];
  const rest: Record<string, unknown> = { ...log.metadata };
  delete rest["changes"];
  delete rest["request"];
  delete rest["client"];

  const fields: [string, string][] = [
    ["ID", log.id],
    ["项目", log.project_id || "（平台级）"],
    ["状态", log.status],
    ["操作者", `${log.actor_kind ?? ""}${log.actor_id ? `: ${log.actor_id}` : ""}`],
    ["资源", log.resource_id || "—"],
    ["IP", log.ip || "—"],
    ["User-Agent", log.user_agent || "—"],
    ["时间", log.created_at ? formatDateTime(log.created_at, tz) : "—"],
  ];

  return (
    <div className="space-y-4 text-sm">
      <div className="grid grid-cols-2 gap-x-4 gap-y-2">
        {fields.map(([k, v]) => (
          <div key={k} className="min-w-0">
            <div className="text-xs text-muted-foreground">{k}</div>
            <div className="truncate font-mono text-xs" title={v}>
              {v}
            </div>
          </div>
        ))}
      </div>

      {changes && Object.keys(changes).length > 0 && (
        <div className="space-y-2">
          <div className="text-xs font-semibold text-muted-foreground">字段变更</div>
          <div className="rounded-md border p-3 space-y-1.5">
            {Object.entries(changes).map(([field, change]) => (
              <div key={field} className="grid grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] items-center gap-2 font-mono text-xs">
                <span>{field}</span>
                <span className="text-muted-foreground">→</span>
                <span>
                  {formatValue(change?.from)} → {formatValue(change?.to)}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}

      {request !== undefined && (
        <div className="space-y-2">
          <div className="text-xs font-semibold text-muted-foreground">
            请求摘要（脱敏后）
          </div>
          <JsonEditor
            value={
              typeof request === "string"
                ? request
                : JSON.stringify(request, null, 2)
            }
            onChange={() => {}}
            disabled
            minHeightClass="min-h-[80px]"
            maxHeightClass="max-h-[240px]"
          />
        </div>
      )}

      {(client !== undefined || Object.keys(rest).length > 0) && (
        <div className="space-y-2">
          <div className="text-xs font-semibold text-muted-foreground">其他元数据</div>
          <JsonEditor
            value={JSON.stringify({ ...(client ? { client } : {}), ...rest }, null, 2)}
            onChange={() => {}}
            disabled
            minHeightClass="min-h-[60px]"
            maxHeightClass="max-h-[160px]"
          />
        </div>
      )}
    </div>
  );
}
