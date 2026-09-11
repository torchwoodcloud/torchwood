import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  listAuditLogs,
  type AuditLog,
} from "@/api/auditLogs";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { PageHeader } from "@/components/PageHeader";
import { EmptyState } from "@/components/EmptyState";
import { LoadingTable } from "@/components/LoadingTable";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

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

type ScopeMode = "project" | "include_platform" | "all_projects";

export function AuditLogsListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const platformAdmin = isPlatformAdmin(role);

  // 过滤条件（变更即回到第一页）。
  const [action, setAction] = useState("");
  const [status, setStatus] = useState("");
  const [actorId, setActorId] = useState("");
  const [resourceId, setResourceId] = useState("");
  const [createdAfter, setCreatedAfter] = useState("");
  const [createdBefore, setCreatedBefore] = useState("");
  const [scope, setScope] = useState<ScopeMode>("project");
  const [pageSize, setPageSize] = useState(50);

  // 服务端分页：token 栈（栈底 = 第一页的空 token）。
  const [tokenStack, setTokenStack] = useState<string[]>([""]);
  const pageToken = tokenStack[tokenStack.length - 1];

  const [selected, setSelected] = useState<AuditLog | null>(null);

  const resetPage = () => setTokenStack([""]);

  const params = useMemo(
    () => ({
      page_size: pageSize,
      page_token: pageToken || undefined,
      action: action || undefined,
      status: status || undefined,
      actor_id: actorId || undefined,
      resource_id: resourceId || undefined,
      created_after: createdAfter ? new Date(createdAfter).toISOString() : undefined,
      created_before: createdBefore ? new Date(createdBefore).toISOString() : undefined,
      include_platform: scope === "include_platform" || scope === "all_projects" || undefined,
      all_projects: scope === "all_projects" || undefined,
    }),
    [pageSize, pageToken, action, status, actorId, resourceId, createdAfter, createdBefore, scope]
  );

  const { data, isLoading } = useQuery({
    queryKey: ["audit-logs", projectId, params],
    queryFn: () => listAuditLogs(params),
    enabled: !!projectId,
  });

  const logs = data?.audit_logs ?? [];
  const meta = data?.meta;

  const goNext = () => {
    if (meta?.next_page_token) {
      setTokenStack((s) => [...s, meta.next_page_token!]);
    }
  };
  const goPrev = () => {
    setTokenStack((s) => (s.length > 1 ? s.slice(0, -1) : s));
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="审计日志"
        description="管理面（Console / CLI / SDK / API Key）全部系统变更的完整审计轨迹"
      />

      <Card>
        <CardHeader className="space-y-4">
          <CardTitle>审计记录</CardTitle>
          <div className="grid grid-cols-1 gap-3 md:grid-cols-3 xl:grid-cols-6">
            <div className="space-y-1">
              <Label htmlFor="audit-action">操作</Label>
              <Input
                id="audit-action"
                placeholder="如 FunctionsService/UpdateFunction"
                value={action}
                onChange={(e) => {
                  setAction(e.target.value);
                  resetPage();
                }}
              />
            </div>
            <div className="space-y-1">
              <Label>状态</Label>
              <Select
                value={status || "all"}
                onValueChange={(v) => {
                  setStatus(v === "all" ? "" : v);
                  resetPage();
                }}
              >
                <SelectTrigger>
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
                value={actorId}
                onChange={(e) => {
                  setActorId(e.target.value);
                  resetPage();
                }}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="audit-resource">资源 ID</Label>
              <Input
                id="audit-resource"
                value={resourceId}
                onChange={(e) => {
                  setResourceId(e.target.value);
                  resetPage();
                }}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="audit-after">起始时间</Label>
              <Input
                id="audit-after"
                type="datetime-local"
                value={createdAfter}
                onChange={(e) => {
                  setCreatedAfter(e.target.value);
                  resetPage();
                }}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="audit-before">截止时间</Label>
              <Input
                id="audit-before"
                type="datetime-local"
                value={createdBefore}
                onChange={(e) => {
                  setCreatedBefore(e.target.value);
                  resetPage();
                }}
              />
            </div>
          </div>
          {platformAdmin && (
            <div className="flex flex-wrap items-center gap-3">
              <div className="w-56 space-y-1">
                <Label>视图范围</Label>
                <Select
                  value={scope}
                  onValueChange={(v) => {
                    setScope(v as ScopeMode);
                    resetPage();
                  }}
                >
                  <SelectTrigger>
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
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <LoadingTable columns={7} />
          ) : logs.length === 0 ? (
            <EmptyState
              title="暂无审计记录"
              description="调整过滤条件，或等待系统变更产生新的审计行"
            />
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>时间</TableHead>
                    <TableHead>变更</TableHead>
                    <TableHead>状态</TableHead>
                    <TableHead>通道</TableHead>
                    <TableHead>操作者</TableHead>
                    <TableHead>资源</TableHead>
                    <TableHead>IP</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {logs.map((log) => {
                    const channel = channelOf(log);
                    return (
                      <TableRow
                        key={log.id}
                        className="cursor-pointer"
                        onClick={() => setSelected(log)}
                      >
                        <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                          {log.created_at
                            ? new Date(log.created_at).toLocaleString()
                            : "—"}
                        </TableCell>
                        <TableCell className="max-w-[320px]">
                          <div className="truncate font-medium" title={describeAudit(log)}>
                            {describeAudit(log)}
                          </div>
                          <div className="truncate font-mono text-xs text-muted-foreground" title={log.action}>
                            {log.action}
                          </div>
                        </TableCell>
                        <TableCell>
                          <Badge variant={statusBadgeVariant(log.status)}>
                            {log.status}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          {channel ? (
                            <Badge variant="outline">
                              {CHANNEL_LABELS[channel] ?? channel}
                            </Badge>
                          ) : (
                            <span className="text-muted-foreground">—</span>
                          )}
                        </TableCell>
                        <TableCell className="max-w-[160px] truncate font-mono text-xs">
                          {log.actor_kind ? `${log.actor_kind}:` : ""}
                          {log.actor_id || "—"}
                        </TableCell>
                        <TableCell className="max-w-[140px] truncate font-mono text-xs">
                          {log.resource_id || "—"}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {log.ip || "—"}
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
              <div className="flex items-center justify-between pt-4">
                <div className="text-sm text-muted-foreground">
                  共 {meta?.total_count ?? "—"} 条 · 每页 {meta?.page_size ?? pageSize} 条
                </div>
                <div className="flex items-center gap-2">
                  <Select
                    value={String(pageSize)}
                    onValueChange={(v) => {
                      setPageSize(Number(v));
                      resetPage();
                    }}
                  >
                    <SelectTrigger className="w-24">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="20">20 条</SelectItem>
                      <SelectItem value="50">50 条</SelectItem>
                      <SelectItem value="100">100 条</SelectItem>
                    </SelectContent>
                  </Select>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={tokenStack.length <= 1}
                    onClick={goPrev}
                  >
                    上一页
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!meta?.next_page_token}
                    onClick={goNext}
                  >
                    下一页
                  </Button>
                </div>
              </div>
            </>
          )}
        </CardContent>
      </Card>

      <Dialog open={!!selected} onOpenChange={(open) => !open && setSelected(null)}>
        <DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
          <DialogHeader>
            <DialogTitle className="font-mono text-base">{selected?.action}</DialogTitle>
          </DialogHeader>
          {selected && <AuditLogDetail log={selected} />}
        </DialogContent>
      </Dialog>
    </div>
  );
}

// AuditLogDetail 渲染详情：全字段 + 结构化变更内容分区（changes / request / client）。
function AuditLogDetail({ log }: { log: AuditLog }) {
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
    ["时间", log.created_at ? new Date(log.created_at).toLocaleString() : "—"],
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
          <div className="rounded-md border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>字段</TableHead>
                  <TableHead>旧值</TableHead>
                  <TableHead>新值</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {Object.entries(changes).map(([field, change]) => (
                  <TableRow key={field}>
                    <TableCell className="font-mono text-xs">{field}</TableCell>
                    <TableCell className="font-mono text-xs">
                      {formatValue(change?.from)}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {formatValue(change?.to)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
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
