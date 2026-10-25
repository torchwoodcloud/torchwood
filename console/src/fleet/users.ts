import {
  defineResource,
  type CollectionQuery,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listUsers, type User } from "@/api/users";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：users 列表换 useCollection——游标/页大小/服务端
// 过滤（ParseUserList 白名单 DSL 的四参）进 URL；数据面复用 axios 管线
//（刷新/项目头），失败在适配器收敛为 ApiError（fleet/errors.ts）。删除等
// 写路径仍走存量 useMutation——kit 列表键失效经 usersCollectionKey 手动桥
//（命令族切片后由 useCommand 自动失效取代）。

// 服务端过滤字段（URL 谓词 eq 叶子）。created_after/created_before 在 URL
// 存绝对时刻（ISO）：跨时区可分享；页面在写入边界做墙钟↔绝对换算（按管理
// 员时区偏好），适配器不涉时区（audit-logs 同款）。
const FILTER_FIELDS = ["id", "status", "created_after", "created_before"] as const;

type FilterField = (typeof FILTER_FIELDS)[number];
export type { FilterField as UserFilterField };

export function userFiltersOf(
  predicate: Predicate | undefined
): Partial<Record<FilterField, string>> {
  if (!predicate) return {};
  const leaves = "all" in predicate ? predicate.all : "field" in predicate ? [predicate] : [];
  const out: Partial<Record<FilterField, string>> = {};
  for (const leaf of leaves) {
    if (!("field" in leaf)) continue;
    const field = leaf.field as FilterField;
    if (
      (FILTER_FIELDS as readonly string[]).includes(field) &&
      typeof leaf.value === "string" &&
      leaf.value !== ""
    ) {
      out[field] = leaf.value;
    }
  }
  return out;
}

// DSL 字符串值消毒：剥离引号/反斜杠/括号，防止 URL 值破坏 equal(...) 语法
//（非法输入只会查不到，不会注入——ParseUserList 白名单编译列名）。
const sanitizeDSLValue = (v: string) => v.replace(/[\\ "']/g, "").trim();

async function fetchUsers(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<User>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const f = userFiltersOf(q.predicate);
    const queries: string[] = [];
    const id = sanitizeDSLValue(f.id ?? "");
    if (id) queries.push(`equal("id","${id}")`);
    if (f.status) queries.push(`equal("status","${sanitizeDSLValue(f.status)}")`);
    if (f.created_after)
      queries.push(`greaterThan("created_at","${sanitizeDSLValue(f.created_after)}")`);
    if (f.created_before)
      queries.push(`lessThan("created_at","${sanitizeDSLValue(f.created_before)}")`);
    const page = await listUsers({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
      ...(queries.length > 0 ? { queries } : {}),
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const usersResource = defineResource({
  name: "users",
  getId: (row: User) => row.id,
  scope: ["projectId"],
  collection: fetchUsers,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；filters = 旧页真实传给服务端的四参（无其他服务端过滤）。
export const usersSearch = collectionQuerySchema({
  filters: FILTER_FIELDS.map((field) => ({ field })),
  defaultSize: 20,
});

// kit 列表键前缀（键形 [kind, 资源名, …]，见 ui-kit keys.ts；前缀匹配即
// 全窗口/全谓词失效）：存量 useMutation 删除后的手动失效桥。
export const usersCollectionKey = ["collection", "users"] as const;
