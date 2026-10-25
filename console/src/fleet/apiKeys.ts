import {
  defineResource,
  type CommandResult,
  type ResourceScope,
} from "@lynx-go/ui-kit/kernel";
import {
  createAPIKey,
  deleteAPIKey,
  updateAPIKey,
  type APIKey,
  type UpdateAPIKeyInput,
} from "@/api/apiKeys";
import { gatewayErrorCode, toApiError } from "@/fleet/errors";

// fleet-ui 命令族切片（api-keys 首个 commands 资源，design.md §12.3）：
// 结果两分映射——400→rejected{code,message}、409→conflict、其余 throw
// ApiError（toApiError 收敛缝）；命令调用一律 __skipToast（toast 单源 =
// onSettled/结果盒，防与 axios 拦截器双 toast）。失效：kit 键自动 +
// 存量 ["api-keys"] 键由页面 onSettled 手动桥接（isFleetKey 守卫）。

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
  op: () => Promise<Payload>,
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

// —— 新建：表单值（展示形）在 run 内收敛为提交形（wildcard/勾选/手输合并）——

export interface CreateApiKeyForm {
  name: string;
  wildcard: boolean;
  /** 词表勾选（form.Field，任意值类型皆可）。 */
  selected: string[];
  /** 手输区（逗号或换行分隔）。 */
  manual: string;
}

export interface CreateApiKeyPayload {
  api_key: APIKey;
  /** 一次性 reveal（useCommandForm.reveal 暴露；卸载即弃）。 */
  secret: string;
}

function mergeScopes(form: CreateApiKeyForm): string[] {
  if (form.wildcard) return ["*"];
  const manual = form.manual
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
  return Array.from(new Set([...form.selected, ...manual])).sort();
}

// —— 编辑：diff 语义归 descriptor（proto3 optional 只传变更字段）——
// 字段值存 ISO（datetime-local 换算在字段边界按管理员时区做），descriptor
// 无时区依赖。

export interface EditApiKeyForm {
  name: string;
  /** 逗号分隔展示形。 */
  scopes: string;
  enabled: boolean;
  /** ISO 或 ""（永不过期/不变）。 */
  expireAt: string;
}

/** descriptor 工厂：闭包原实体快照做字段 diff（编辑对话框 useCommandForm 用）。 */
export function editApiKeyDescriptor(key: APIKey) {
  return {
    run: async (form: EditApiKeyForm): Promise<CommandResult<APIKey>> =>
      runCommandMapped(async () => {
        const input: UpdateAPIKeyInput = {};
        if (form.name !== key.name) input.name = form.name;
        const nextScopes = form.scopes
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean);
        if (nextScopes.join(",") !== key.scopes.join(",")) input.scopes = nextScopes;
        if (form.enabled !== key.enabled) input.enabled = form.enabled;
        if (
          form.expireAt &&
          (!key.expire_at || form.expireAt !== new Date(key.expire_at).toISOString())
        ) {
          input.expire_at = form.expireAt;
        }
        return updateAPIKey(key.id, input, SKIP);
      }),
    // delete 已删 → 幂等 ok 同族语义：编辑目标消失视为已达成
    treatAs: { "not-found": { status: "ok" as const, payload: key } },
  };
}

// —— 轮换第二步：旧 Key 立即过期（原地 update，输入自包含）——

export const expireApiKeyDescriptor = {
  run: async (input: { id: string }): Promise<CommandResult<APIKey>> =>
    runCommandMapped(() => updateAPIKey(input.id, { expire_at: new Date().toISOString() }, SKIP)),
};

export const apiKeysResource = defineResource({
  name: "api-keys",
  getId: (row: APIKey) => row.id,
  scope: ["projectId"],
  commands: {
    // 表单值→提交形收敛在 run 内（wildcard/勾选/手输合并去重排序）
    create: {
      run: async (form: CreateApiKeyForm): Promise<CommandResult<CreateApiKeyPayload>> =>
        runCommandMapped(() => createAPIKey({ name: form.name, scopes: mergeScopes(form) }, SKIP)),
    },
    // update 不进资源 commands：它需要原实体快照做 diff（输入不自包含），
    // 经 editApiKeyDescriptor(key) 工厂按行构造、页面级 useCommandForm 消费
    //（失效走存量 ["api-keys"] 手动桥——本切片所有消费方都是存量键）。
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteAPIKey(input.id, SKIP);
          return null;
        }),
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
    expireOld: expireApiKeyDescriptor,
  },
});

// 命令句柄提升（demo 同款 commands! 模式——Resource.commands 可选类型面）。
export const createApiKeyCommand = apiKeysResource.commands!.create;
export const removeApiKeyCommand = apiKeysResource.commands!.remove;

// 资源 scope 约定（页面 useCommand 传 {projectId}）。
export type ApiKeysScope = ResourceScope;
