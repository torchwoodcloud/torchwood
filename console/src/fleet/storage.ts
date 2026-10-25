import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import {
  listBuckets,
  createBucket,
  updateBucket,
  deleteBucket,
  uploadFile,
  updateFile,
  deleteFile,
  type Bucket,
  type FileItem,
} from "@/api/storage";
import { gatewayErrorCode, toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：Bucket 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（fleet/errors.ts 收敛缝）。
//
// 命令族切片（§12.3）：桶建/改/删挂资源 commands（kit 集合键由 useCommand
// 自动失效）；文件是桶子资源、无 kit 集合——独立 descriptor，失效由页面
// 桥存量 ["files"] 键。结果两分映射同 apiKeys 先例（400→rejected、
// 409→conflict、其余 throw ApiError）；命令调用一律 __skipToast（toast
// 单源 = onSettled）。

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

/** 结果两分映射：400→rejected、409→conflict、其余 throw（toApiError 收敛）。 */
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

async function fetchBuckets(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Bucket>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listBuckets({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const bucketsResource = defineResource({
  name: "buckets",
  getId: (row: Bucket) => row.id,
  scope: ["projectId"],
  collection: fetchBuckets,
  commands: {
    create: {
      run: async (input: { name: string; public: boolean }): Promise<CommandResult<Bucket>> =>
        runCommandMapped(() => createBucket(input, SKIP)),
    },
    update: {
      run: async (input: {
        id: string;
        name?: string;
        public?: boolean;
      }): Promise<CommandResult<Bucket>> =>
        runCommandMapped(() => updateBucket(input.id, { name: input.name, public: input.public }, SKIP)),
    },
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteBucket(input.id, SKIP);
          return null;
        }),
      // delete 已删 → 幂等 ok 同族语义（apiKeys remove 先例）
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
  },
});

// 命令句柄提升（demo 同款 commands! 模式——Resource.commands 可选类型面）。
export const createBucketCommand = bucketsResource.commands!.create;
export const updateBucketCommand = bucketsResource.commands!.update;
export const removeBucketCommand = bucketsResource.commands!.remove;

// ——文件子资源命令（独立 descriptor：不挂桶资源——文件写不改变桶集合，
// 失效由页面桥存量 ["files", bucketId] 键）——

export const uploadFileDescriptor = {
  run: async (input: { bucketId: string; file: File }): Promise<CommandResult<FileItem>> =>
    runCommandMapped(() => uploadFile(input.bucketId, input.file, SKIP)),
};

export const removeFileDescriptor = {
  run: async (input: { bucketId: string; fileId: string }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await deleteFile(input.bucketId, input.fileId, SKIP);
      return null;
    }),
  treatAs: { "not-found": { status: "ok" as const, payload: null } },
};

export const renameFileDescriptor = {
  run: async (input: {
    bucketId: string;
    fileId: string;
    name: string;
  }): Promise<CommandResult<FileItem>> =>
    runCommandMapped(() => updateFile(input.bucketId, input.fileId, { name: input.name }, SKIP)),
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；桶列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const bucketsSearch = collectionQuerySchema({ defaultSize: 20 });
