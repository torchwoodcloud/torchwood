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
  listFunctions,
  createFunction,
  updateFunction,
  deleteFunction,
  rotateFunctionSecret,
  setFunctionScopes,
  invokeFunction,
  createFunctionTrigger,
  deleteFunctionTrigger,
  rotateFunctionTriggerToken,
  type FunctionItem,
  type FunctionSecret,
  type CreateFunctionResult,
  type CreateTriggerInput,
  type FunctionTrigger,
  type Invocation,
} from "@/api/functions";
import { runCommandMapped, toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：Functions 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（fleet/errors.ts 收敛缝）。
//
// 命令族切片（§12.3）：建/改/删/轮换/执行身份挂资源 commands（kit 集合键由
// useCommand 自动失效）；invoke 是动作（非实体变更）——挂资源但显式
// invalidates: [] 不失效函数集合，存量 ["invocations"] 调用记录键经资源
// legacyKeys 桥（§12.10-② expireOld 样板：挂载句柄注入生效）。结果两分
// 映射同 apiKeys 先例（400→rejected{code,message}、409→conflict、其余
// throw ApiError）；命令调用一律 __skipToast（toast 单源 = onSettled）。
// setScopes 的多行文本（表单展示形）在 run 内收敛为提交形。

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

// invoke 是调试/联调动作：不改变函数实体——invalidates: [] 显式不失效
// 函数集合（kit 键）；存量 ["invocations"] 调用记录键经资源 legacyKeys 桥。
// 挂资源（expireOld 样板）仅为让 legacyKeys 注入生效——页面用挂载句柄。
export const invokeFunctionDescriptor = {
  run: async (input: {
    id: string;
    data: string;
    async: boolean;
  }): Promise<CommandResult<Invocation>> =>
    runCommandMapped(() => invokeFunction(input.id, { data: input.data, async: input.async }, SKIP)),
  invalidates: [],
};

export const functionsResource = defineResource({
  name: "functions",
  getId: (row: FunctionItem) => row.id,
  scope: ["projectId"],
  collection: fetchFunctions,
  // 存量键失效桥（§12.10-②）：命令 ok 后除 kit 键外失效存量 ["functions"]
  // /["invocations"] 前缀键（宽前缀覆盖 projectId/functionId 形）——页面
  // onSettled 手动桥退役。
  legacyKeys: () => [["functions"], ["invocations"]],
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
    // 客户端调用策略（function 实体的 client_* 面）：配额为文本展示形，
    // run 内收敛整数（非法/负数回退 0；callable=true 要求 ≥1 由服务端校验）。
    updateClientPolicy: {
      run: async (input: {
        id: string;
        client_callable: boolean;
        /** 展示形（文本）。 */
        clientPerUserLimit: string;
        client_limit_window: "minute" | "hour" | "day";
      }): Promise<CommandResult<FunctionItem>> =>
        runCommandMapped(() => {
          const parsed = Number.parseInt(input.clientPerUserLimit, 10);
          return updateFunction(
            input.id,
            {
              client_callable: input.client_callable,
              client_per_user_limit: Number.isFinite(parsed) && parsed >= 0 ? parsed : 0,
              client_limit_window: input.client_limit_window,
            },
            SKIP
          );
        }),
    },
    // 动作（非实体变更）：invalidates: [] 在 descriptor 内声明（见上）。
    invoke: invokeFunctionDescriptor,
  },
});

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createFunctionCommand = functionsResource.commands.create;
export const updateFunctionCommand = functionsResource.commands.update;
export const removeFunctionCommand = functionsResource.commands.remove;
export const rotateFunctionSecretCommand = functionsResource.commands.rotateSecret;
export const setFunctionScopesCommand = functionsResource.commands.setScopes;
export const updateFunctionClientPolicyCommand = functionsResource.commands.updateClientPolicy;
export const invokeFunctionCommand = functionsResource.commands.invoke;

// ——触发器（function 子资源，users 会话撤销同款先例）：独立 descriptor
// 不挂资源——触发器集合不是 functions 列表语义；存量 ["function-triggers"]
// 前缀键在 descriptor 级 legacyKeys 声明（§12.10-②，宽前缀覆盖
// ["function-triggers", functionId] 形）——页面 onSettled 手动桥退役。
// 创建的表单展示形（KB 文本/条件勾选/多行订阅串）在 run 内收敛为 oneof
// 提交形。

export interface CreateTriggerForm {
  type: "http" | "cron" | "event";
  // http 展示形。
  responseMode: string;
  /** async_ack 的回执体（空 = 不发送）。 */
  ackBody: string;
  handshake: boolean;
  /** body 上限（KB 文本；run 内换算字节，非法/≤0 不发送）。 */
  bodyLimitKB: string;
  // cron 展示形。
  expr: string;
  misfire: string;
  /** event 展示形：多行订阅串，一行一条。 */
  eventsText: string;
}

export interface TriggerCreateInput extends CreateTriggerForm {
  functionId: string;
}

function buildTriggerInput(form: CreateTriggerForm): CreateTriggerInput {
  if (form.type === "http") {
    const bodyLimit = Number.parseInt(form.bodyLimitKB, 10);
    return {
      type: form.type,
      http: {
        response_mode: form.responseMode,
        ...(form.responseMode === "async_ack" && form.ackBody.trim() !== ""
          ? { ack_body: form.ackBody }
          : {}),
        ...(form.handshake ? { handshake: "echo" } : {}),
        ...(Number.isFinite(bodyLimit) && bodyLimit > 0
          ? { body_limit_bytes: bodyLimit * 1024 }
          : {}),
      },
    };
  }
  if (form.type === "cron") {
    return { type: form.type, cron: { expr: form.expr, misfire: form.misfire } };
  }
  return {
    type: form.type,
    event: {
      events: form.eventsText
        .split("\n")
        .map((line) => line.trim())
        .filter((line) => line !== ""),
    },
  };
}

export const createTriggerDescriptor = {
  run: async (input: TriggerCreateInput): Promise<CommandResult<FunctionTrigger>> =>
    runCommandMapped(() =>
      createFunctionTrigger(input.functionId, buildTriggerInput(input), SKIP)
    ),
  legacyKeys: () => [["function-triggers"]],
};

export const removeTriggerDescriptor = {
  run: async (input: {
    functionId: string;
    triggerId: string;
  }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await deleteFunctionTrigger(input.functionId, input.triggerId, SKIP);
      return null;
    }),
  // 已删 → 幂等 ok（apiKeys remove 先例）
  treatAs: { "not-found": { status: "ok" as const, payload: null } },
  legacyKeys: () => [["function-triggers"]],
};

export const rotateTriggerTokenDescriptor = {
  run: async (input: {
    functionId: string;
    triggerId: string;
  }): Promise<CommandResult<FunctionTrigger>> =>
    runCommandMapped(() => rotateFunctionTriggerToken(input.functionId, input.triggerId, SKIP)),
  legacyKeys: () => [["function-triggers"]],
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；函数列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const functionsSearch = collectionQuerySchema({ defaultSize: 20 });
