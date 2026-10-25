import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import {
  createCollection,
  createDatabase,
  deleteCollection,
  deleteDatabase,
  listCollections,
  listDatabases,
  type Collection,
  type Database,
} from "@/api/databases";
import { gatewayErrorCode, toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2，databases 域）：databases 顶层列表 + Database 详情
// 内嵌的 collections 列表。游标/页大小进 URL（分页栈 = 浏览器历史）；两个
// 端点均无真实服务端过滤参数（仅分页）→ schema 不设 filters/text，页内搜索
// 是本地态投影不进 URL。数据面复用 axios 管线（刷新/项目头），失败在适配器
// 收敛为 ApiError（fleet/errors.ts）。
//
// 建删命令族切片：database/collection 的 create/remove 挂各自资源（kit 列表
// 键随 ok 自动失效）；存量 ["databases"] / ["collections"] 键由页面 onSettled
// 手动桥。结果两分映射同 apiKeys（400→rejected、409→conflict、其余 throw
// ApiError）；api 调用一律 __skipToast（toast 单源 = onSettled）。

// 网关错误体（结果分支用）。
interface GatewayErrorBody {
  error?: { code?: string; message?: string };
}

function gatewayMessage(err: unknown): string {
  if (
    typeof err === "object" &&
    err !== null &&
    "response" in err &&
    (err as { response?: { data?: GatewayErrorBody } }).response?.data?.error?.message
  ) {
    return String((err as { response: { data: GatewayErrorBody } }).response.data.error!.message);
  }
  return err instanceof Error ? err.message : String(err);
}

function isStatus(err: unknown, status: number): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    "response" in err &&
    (err as { response?: { status?: number } }).response?.status === status
  );
}

/** 结果两分映射：400→rejected、409→conflict、404 按 treatAs 由调用方裁决、其余 throw。 */
async function runCommandMapped<Payload>(
  op: () => Promise<Payload>
): Promise<CommandResult<Payload>> {
  try {
    return { status: "ok", payload: await op() };
  } catch (err) {
    if (isStatus(err, 400)) {
      return { status: "rejected", code: gatewayErrorCode(err), message: gatewayMessage(err) };
    }
    if (isStatus(err, 409)) {
      return { status: "conflict", message: gatewayMessage(err) };
    }
    throw toApiError(err);
  }
}

const SKIP = { __skipToast: true } as const;

// —— databases（顶层列表；scope 只有 projectId）——

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页默认（useServerPaging 缺省）。
export const databasesSearch = collectionQuerySchema({ defaultSize: 20 });

async function fetchDatabases(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Database>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listDatabases({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const databasesResource = defineResource({
  name: "databases",
  getId: (row: Database) => row.id,
  scope: ["projectId"],
  collection: fetchDatabases,
  commands: {
    create: {
      run: async (input: { id: string; name: string }): Promise<CommandResult<Database>> =>
        runCommandMapped(() => createDatabase(input, SKIP)),
    },
    // 删除已删 → 幂等 ok（apiKeys/documents remove 先例）
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteDatabase(input.id, SKIP);
          return null;
        }),
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
  },
});

// —— collections（Database 详情内嵌列表；嵌套 scope 加 dbId，ctx 缺参静默空段）——

// 集合新建：schema 定义表单（名称/ID/权限）展示形在 run 内收敛——ID 空则从
// 名称派生（小写 + 空白→下划线），权限空数组不发送（走服务端默认策略）。
export interface CreateCollectionForm {
  dbId: string;
  /** 留空 = 从名称派生。 */
  id: string;
  name: string;
  permissions: string[];
}

export const collectionsSearch = collectionQuerySchema({ defaultSize: 20 });

async function fetchCollections(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Collection>> {
  if (ctx.projectId == null || ctx.dbId == null) return { rows: [] };
  try {
    const page = await listCollections(ctx.dbId, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const collectionsResource = defineResource({
  name: "collections",
  getId: (row: Collection) => row.id,
  scope: ["projectId", "dbId"],
  collection: fetchCollections,
  commands: {
    create: {
      run: async (form: CreateCollectionForm): Promise<CommandResult<Collection>> =>
        runCommandMapped(() =>
          createCollection(
            form.dbId,
            {
              id: form.id || form.name.toLowerCase().replace(/\s+/g, "_"),
              name: form.name,
              permissions: form.permissions.length > 0 ? form.permissions : undefined,
            },
            SKIP
          )
        ),
    },
    // 删除已删 → 幂等 ok
    remove: {
      run: async (input: {
        dbId: string;
        collId: string;
      }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteCollection(input.dbId, input.collId, SKIP);
          return null;
        }),
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
  },
});

// 命令句柄提升（demo 同款 commands! 模式——Resource.commands 可选类型面）。
export const createDatabaseCommand = databasesResource.commands!.create;
export const removeDatabaseCommand = databasesResource.commands!.remove;
export const createCollectionCommand = collectionsResource.commands!.create;
export const removeCollectionCommand = collectionsResource.commands!.remove;
