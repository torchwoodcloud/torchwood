import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { skipToast as SKIP } from "@/api/client";
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
import { runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：Bucket 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（kit/errors.ts 收敛缝）。
//
// 命令族切片（§12.3）：桶建/改/删挂资源 commands（kit 集合键由 useCommand
// 自动失效）；文件是桶子资源、无 kit 集合——独立 descriptor，descriptor 级
// legacyKeys 声明存量 ["files"] 前缀键桥（§12.10-②，宽前缀覆盖
// ["files", bucketId] 全族）。结果两分映射同 apiKeys 先例（400→rejected、
// 409→conflict、其余 throw ApiError）；命令调用一律 __skipToast（toast
// 单源 = onSettled）。

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
  // 存量键失效桥（§12.10-②）：命令 ok 后除 kit 键外失效存量 ["buckets"]/
  // ["storage-usage"] 前缀键——页面 onSettled 手动桥退役。
  legacyKeys: () => [["buckets"], ["storage-usage"]],
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

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createBucketCommand = bucketsResource.commands.create;
export const updateBucketCommand = bucketsResource.commands.update;
export const removeBucketCommand = bucketsResource.commands.remove;

// ——文件子资源命令（独立 descriptor：不挂桶资源——文件写不改变桶集合；
// 存量 ["files"] 前缀键桥在 descriptor 级声明，宽前缀覆盖
// ["files", bucketId] 全族——页面手动桥退役）——

export const uploadFileDescriptor = {
  run: async (input: { bucketId: string; file: File }): Promise<CommandResult<FileItem>> =>
    runCommandMapped(() => uploadFile(input.bucketId, input.file, SKIP)),
  legacyKeys: () => [["files"]],
};

export const removeFileDescriptor = {
  run: async (input: { bucketId: string; fileId: string }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await deleteFile(input.bucketId, input.fileId, SKIP);
      return null;
    }),
  treatAs: { "not-found": { status: "ok" as const, payload: null } },
  legacyKeys: () => [["files"]],
};

export const renameFileDescriptor = {
  run: async (input: {
    bucketId: string;
    fileId: string;
    name: string;
  }): Promise<CommandResult<FileItem>> =>
    runCommandMapped(() => updateFile(input.bucketId, input.fileId, { name: input.name }, SKIP)),
  legacyKeys: () => [["files"]],
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；桶列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const bucketsSearch = collectionQuerySchema({ defaultSize: 20 });
