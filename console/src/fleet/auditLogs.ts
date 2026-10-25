import {
  defineResource,
  type CollectionQuery,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listAuditLogs, type AuditLog } from "@/api/auditLogs";
import { toApiError } from "@/fleet/errors";

// fleet-ui keyset 切片（audit-logs 首个资源）：URL 即真相（q/filter/sort/
// cursor/size），useCollection 消费；分页栈 = 浏览器历史（KeysetPagination
// 的 Prev 在不可算游标时走 history.back）。数据面复用 axios 管线（刷新/
// 项目头），失败在适配器收敛为 ApiError（fleet/errors.ts）。

// 谓词 eq 叶子 → ListAuditLogsParams 键的白名单；scope 是伪字段（视图
// 范围三态），由适配器展开为 include_platform/all_projects 布尔参数。
const FILTER_FIELDS = [
  "action",
  "status",
  "actor_id",
  "resource_id",
  "created_after",
  "created_before",
  "scope",
] as const;

type FilterField = (typeof FILTER_FIELDS)[number];
export type { FilterField };

export function filtersOf(predicate: Predicate | undefined): Partial<Record<FilterField, string>> {
  if (!predicate) return {};
  const leaves = "all" in predicate ? predicate.all : "field" in predicate ? [predicate] : [];
  const out: Partial<Record<FilterField, string>> = {};
  for (const leaf of leaves) {
    if (!("field" in leaf)) continue;
    const field = leaf.field as FilterField;
    if ((FILTER_FIELDS as readonly string[]).includes(field) && typeof leaf.value === "string") {
      out[field] = leaf.value;
    }
  }
  return out;
}

// created_after/created_before 在 URL 存绝对时刻（ISO）：跨时区可分享；
// 页面在写入边界做墙钟↔绝对换算（按管理员时区偏好），适配器不涉时区。
async function fetchAuditLogs(
  q: CollectionQuery,
  ctx: ResourceScope,
  signal?: AbortSignal,
): Promise<Segment<AuditLog>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const f = filtersOf(q.predicate);
    // 无 order = 服务端历史默认 DESC（不设 schema defaultOrder：DataTable
    // 的 none→asc→desc 循环要能落回"无排序"态，默认序会把它钉死在 desc）。
    const dir = q.order?.[0]?.field === "created_at" ? q.order[0].dir : undefined;
    const res = await listAuditLogs(
      {
        page_size: q.window.size,
        page_token: q.window.cursor || undefined,
        action: f.action,
        status: f.status,
        actor_id: f.actor_id,
        resource_id: f.resource_id,
        created_after: f.created_after,
        created_before: f.created_before,
        include_platform:
          f.scope === "include_platform" || f.scope === "all_projects" || undefined,
        all_projects: f.scope === "all_projects" || undefined,
        sort_order: dir === "asc" ? "ASC" : dir === "desc" ? "DESC" : undefined,
      },
      signal,
    );
    return {
      rows: res.audit_logs,
      // audit-logs 网关附带 total_count（keyset 带计数的方言）；其余列表
      // 端点无 total——Segment.total 可缺省，块按缺省降级展示。
      total: res.meta?.total_count,
      nextCursor: res.meta?.next_page_token,
    };
  } catch (err) {
    throw toApiError(err);
  }
}

export const auditLogsResource = defineResource({
  name: "audit-logs",
  getId: (row: AuditLog) => row.id,
  scope: ["projectId"],
  collection: fetchAuditLogs,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 50 = 旧页默认；无文本快筛（服务端全为精确匹配参数）。
export const auditLogsSearch = collectionQuerySchema({
  filters: FILTER_FIELDS.map((field) => ({ field })),
  defaultSize: 50,
});
