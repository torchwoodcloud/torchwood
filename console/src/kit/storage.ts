import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  listBuckets,
  listFiles,
  getBucket,
  getFile,
  getStorageUsage,
  createBucket,
  updateBucket,
  deleteBucket,
  uploadFile,
  updateFile,
  deleteFile,
  type Bucket,
  type FileItem,
  type StorageUsage,
} from "@/api/storage";
import { recordOrNull, runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：Bucket 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（kit/errors.ts 收敛缝）。
//
// 命令族切片（§12.3）：桶建/改/删挂资源 commands（kit 集合键由 useCommand
// 自动失效）。子视图 kit 化批：桶详情读挂 bucketsResource.record；文件升格
// files 资源（分页 collection + record 槽），项目用量单例读升格 storage-usage
// 资源（record 槽）；文件写命令改声明 invalidates [{resource:"files"}]（kit
// 键全 kind 命中），存量前缀桥（§12.10-②）随读者清零退役。结果两分映射同
// apiKeys 先例（400→rejected、409→conflict、其余 throw ApiError）；命令调用
// 一律 __skipToast（toast 单源 = onSettled）。

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
  // 详情读（BucketDetailPage，子视图 kit 化批）：404 → null 空态直出
  //（recordOrNull 手写方言，不引 restRecord）。
  record: (id) => recordOrNull(() => getBucket(id)) as Promise<Bucket>,
  // 存量键失效桥已退役（子视图 kit 化批）：["buckets"] / ["storage-usage"]
  // 前缀读者清零（桶详情换轨 kit record 键、用量读换轨 storage-usage 资源）；
  // 建删对用量的连带失效由 create/remove 显式 invalidates 承接（见下）。
  commands: {
    // 建删改变项目用量统计——连带失效 storage-usage（invalidates 声明优先 =
    // 整体替换缺省，同资源失效显式带上）。update 不动用量，维持缺省同资源。
    create: {
      invalidates: [{ resource: "buckets" }, { resource: "storage-usage" }],
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
      invalidates: [{ resource: "buckets" }, { resource: "storage-usage" }],
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

// —— files（桶详情内嵌文件列表；嵌套 scope 加 bucketId，ctx 缺参静默空段）——

async function fetchFiles(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<FileItem>> {
  if (ctx.projectId == null || ctx.bucketId == null) return { rows: [] };
  try {
    const page = await listFiles(ctx.bucketId, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const filesResource = defineResource({
  name: "files",
  getId: (row: FileItem) => row.id,
  scope: ["projectId", "bucketId"],
  collection: fetchFiles,
  // 详情读（FileDetailPage，子视图 kit 化批）：404 → null 空态直出；
  // ctx.bucketId 由 scopeReady 就绪门保证（查询未就绪即停用），! 仅类型收窄。
  record: (id, ctx) =>
    recordOrNull(() => getFile(ctx.bucketId!, id)) as Promise<FileItem>,
});

// —— storage-usage（项目级用量单例读；端点无参，id 用 "current" 哨兵占位；
// projectId 经 scope 进键——项目分离保真）——

export const storageUsageResource = defineResource({
  name: "storage-usage",
  scope: ["projectId"],
  record: () => recordOrNull(() => getStorageUsage()) as Promise<StorageUsage>,
});

// ——文件子资源命令（独立 descriptor：不挂桶资源——文件写不改变桶集合；
// invalidates 指向 files 资源 kit 键全 kind，列表/详情一并刷新）——
// 注意：descriptor 无 scopeOf/__scopeKeys——调用点（useCommand 显式 scope 或
// invalidateAfterCommand 直调）必须传全量 { projectId, bucketId }，空 scope
// 投影对声明键集必 miss。

export const uploadFileDescriptor = {
  run: async (input: { bucketId: string; file: File }): Promise<CommandResult<FileItem>> =>
    runCommandMapped(() => uploadFile(input.bucketId, input.file, SKIP)),
  invalidates: [{ resource: "files" }],
};

export const removeFileDescriptor = {
  run: async (input: { bucketId: string; fileId: string }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await deleteFile(input.bucketId, input.fileId, SKIP);
      return null;
    }),
  treatAs: { "not-found": { status: "ok" as const, payload: null } },
  invalidates: [{ resource: "files" }],
};

export const renameFileDescriptor = {
  run: async (input: {
    bucketId: string;
    fileId: string;
    name: string;
  }): Promise<CommandResult<FileItem>> =>
    runCommandMapped(() => updateFile(input.bucketId, input.fileId, { name: input.name }, SKIP)),
  invalidates: [{ resource: "files" }],
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；桶列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const bucketsSearch = collectionQuerySchema({ defaultSize: 20 });
