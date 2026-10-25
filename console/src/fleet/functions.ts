import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import {
  listFunctions,
  createFunction,
  updateFunction,
  deleteFunction,
  rotateFunctionSecret,
  setFunctionScopes,
  invokeFunction,
  type FunctionItem,
  type FunctionSecret,
  type CreateFunctionResult,
  type Invocation,
} from "@/api/functions";
import { gatewayErrorCode, toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：Functions 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（fleet/errors.ts 收敛缝）。
//
// 命令族切片（§12.3）：建/改/删/轮换/执行身份挂资源 commands（kit 集合键由
// useCommand 自动失效）；invoke 是动作（非实体变更）——独立 descriptor，
// 不触发集合失效。结果两分映射同 apiKeys 先例（400→rejected{code,message}、
// 409→conflict、其余 throw ApiError）；命令调用一律 __skipToast（toast 单源
// = onSettled）。setScopes 的多行文本（表单展示形）在 run 内收敛为提交形。

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

async function fetchFunctions(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<FunctionItem>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listFunctions({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const functionsResource = defineResource({
  name: "functions",
  getId: (row: FunctionItem) => row.id,
  scope: ["projectId"],
  collection: fetchFunctions,
  commands: {
    // 创建输入 = 现页面提交形（timeout 页内预检）；payload 携带一次性
    // target_secret（页面 reveal，关闭即弃）。
    create: {
      run: async (input: {
        id: string;
        name: string;
        timeout_seconds: number;
        enabled: boolean;
        target_url: string;
      }): Promise<CommandResult<CreateFunctionResult>> =>
        runCommandMapped(() => createFunction(input, SKIP)),
    },
    update: {
      // target_url 未设置 = 不修改（proto3 optional；diff 归调用方——
      // 页面持原实体快照）。
      run: async (input: {
        id: string;
        name?: string;
        timeout_seconds?: number;
        enabled?: boolean;
        target_url?: string;
      }): Promise<CommandResult<FunctionItem>> =>
        runCommandMapped(() => {
          const { id, ...rest } = input;
          return updateFunction(id, rest, SKIP);
        }),
    },
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteFunction(input.id, SKIP);
          return null;
        }),
      // delete 已删 → 幂等 ok 同族语义（apiKeys remove 先例）
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
    rotateSecret: {
      run: async (input: { id: string }): Promise<CommandResult<FunctionSecret>> =>
        runCommandMapped(() => rotateFunctionSecret(input.id, SKIP)),
    },
    // 表单值→提交形收敛在 run 内（多行文本 split/trim/filter，同 apiKeys
    // mergeScopes 先例）
    setScopes: {
      run: async (input: { id: string; scopesText: string }): Promise<CommandResult<FunctionItem>> =>
        runCommandMapped(() =>
          setFunctionScopes(
            input.id,
            input.scopesText
              .split("\n")
              .map((s) => s.trim())
              .filter((s) => s !== ""),
            SKIP
          )
        ),
    },
  },
});

// 命令句柄提升（demo 同款 commands! 模式——Resource.commands 可选类型面）。
export const createFunctionCommand = functionsResource.commands!.create;
export const updateFunctionCommand = functionsResource.commands!.update;
export const removeFunctionCommand = functionsResource.commands!.remove;
export const rotateFunctionSecretCommand = functionsResource.commands!.rotateSecret;
export const setFunctionScopesCommand = functionsResource.commands!.setScopes;

// invoke 是调试/联调动作：不改变函数实体——独立 descriptor（无集合失效；
// 调用记录由页面桥存量 ["invocations"] 键）。
export const invokeFunctionDescriptor = {
  run: async (input: {
    id: string;
    data: string;
    async: boolean;
  }): Promise<CommandResult<Invocation>> =>
    runCommandMapped(() => invokeFunction(input.id, { data: input.data, async: input.async }, SKIP)),
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；函数列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const functionsSearch = collectionQuerySchema({ defaultSize: 20 });
