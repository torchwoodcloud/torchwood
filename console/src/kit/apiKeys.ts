import {
  type CollectionQuery,
  defineResource,
  type CommandResult,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import {
  createAPIKey,
  deleteAPIKey,
  listAPIKeys,
  updateAPIKey,
  type APIKey,
  type UpdateAPIKeyInput,
} from "@/api/apiKeys";
import { skipToast as SKIP } from "@/api/client";
import { runCommandMapped, toApiError } from "@/kit/errors";

// fleet-ui 命令族切片（api-keys 首个 commands 资源，design.md §12.3）：
// 结果两分映射——400→rejected{code,message}、409→conflict、其余 throw
// ApiError（toApiError 收敛缝）；命令调用一律 __skipToast（toast 单源 =
// onSettled/结果盒，防与 axios 拦截器双 toast）。失效：kit 键自动 +
// 存量 ["api-keys"] 键由页面 onSettled 手动桥接（isFleetKey 守卫）。

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
    // 裸 descriptor 的失效声明（§12.10-②）：kit 键 + 存量 ["api-keys"] 前缀
    invalidates: [{ resource: "api-keys" }],
    legacyKeys: () => [["api-keys"]],
  };
}

// —— 轮换第二步：旧 Key 立即过期（原地 update，输入自包含）——

export const expireApiKeyDescriptor = {
  run: async (input: { id: string }): Promise<CommandResult<APIKey>> =>
    runCommandMapped(() => updateAPIKey(input.id, { expire_at: new Date().toISOString() }, SKIP)),
};

// URL 搜索 schema（路由 validateSearch 与绑定共用）。默认页大小 20 = 旧页
// 默认；enabled 是真实服务端过滤参数（proto3 optional）→ URL 谓词。
export const apiKeysSearch = collectionQuerySchema({
  filters: [{ field: "enabled" }],
  defaultSize: 20,
});

type EqLeaf = { field: string; op: string; value: unknown };

function leavesOf(predicate: Predicate | undefined): EqLeaf[] {
  if (predicate == null) return [];
  if ("all" in predicate) return predicate.all.flatMap((l) => leavesOf(l));
  if ("field" in predicate) return [predicate];
  return [];
}

export function enabledOf(predicate: Predicate | undefined): boolean | undefined {
  const raw = leavesOf(predicate).find((l) => l.field === "enabled")?.value;
  return raw === "true" ? true : raw === "false" ? false : undefined;
}

async function fetchApiKeys(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<APIKey>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listAPIKeys({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
      enabled: enabledOf(q.predicate),
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const apiKeysResource = defineResource({
  name: "api-keys",
  getId: (row: APIKey) => row.id,
  scope: ["projectId"],
  collection: fetchApiKeys,
  // 存量键失效桥（§12.10-②）：命令 ok 后除 kit 键外失效存量 ["api-keys"]
  // 前缀键——页面 onSettled 手动桥退役。
  legacyKeys: () => [["api-keys"]],
  commands: {
    // 表单值→提交形收敛在 run 内（wildcard/勾选/手输合并去重排序）
    create: {
      run: async (form: CreateApiKeyForm): Promise<CommandResult<CreateApiKeyPayload>> =>
        runCommandMapped(() => createAPIKey({ name: form.name, scopes: mergeScopes(form) }, SKIP)),
    },
    // update 不进资源 commands：它需要原实体快照做 diff（输入不自包含），
    // 经 editApiKeyDescriptor(key) 工厂按行构造、页面级 useCommandForm 消费
    //（失效 = invalidates 声明 + 存量键 legacyKeys，见工厂内）。
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

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createApiKeyCommand = apiKeysResource.commands.create;
export const removeApiKeyCommand = apiKeysResource.commands.remove;
export const expireOldApiKeyCommand = apiKeysResource.commands.expireOld;

// 资源 scope 约定（页面 useCommand 传 {projectId}）。
export type ApiKeysScope = ResourceScope;
