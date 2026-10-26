import {
  defineResource,
  type CollectionQuery,
  type CommandResult,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema, filterValues } from "@lynx-go/ui-kit/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  createUser,
  createUserToken,
  deleteUser,
  deleteUserSession,
  listUsers,
  updateUser,
  updateUserPassword,
  type TokenBundle,
  type User,
} from "@/api/users";
import { runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）+ 命令族切片（§12.3，apiKeys 同款）：users 列表
// 换 useCollection——游标/页大小/服务端过滤（ParseUserList 白名单 DSL 的
// 四参）进 URL；数据面复用 axios 管线（刷新/项目头），失败在适配器收敛为
// ApiError（kit/errors.ts）。写路径换 useCommand：结果两分映射——
// 400→rejected、409→conflict、其余 throw ApiError；命令调用一律
// __skipToast（toast 单源 = onSettled）；kit 列表键自动失效（descriptor 挂
// resource），存量键（["users", …]——详情/编辑/会话页仍吃存量键）由
// resource.legacyKeys 宽前缀声明失效（§12.10-②，页面手动桥已退役）。
// 会话撤销/令牌签发不涉 users 集合语义 → 裸 descriptor（不挂 resource，
// 不触发无谓的列表失效；会话键桥在 descriptor 自带 legacyKeys）。

// 服务端过滤字段（URL 谓词 eq 叶子）。created_after/created_before 在 URL
// 存绝对时刻（ISO）：跨时区可分享；页面在写入边界做墙钟↔绝对换算（按管理
// 员时区偏好），适配器不涉时区（audit-logs 同款）。
const FILTER_FIELDS = ["id", "status", "created_after", "created_before"] as const;

type FilterField = (typeof FILTER_FIELDS)[number];
export type { FilterField as UserFilterField };

export function userFiltersOf(
  predicate: Predicate | undefined
): Partial<Record<FilterField, string>> {
  // kit 纯函数投影（schema/filters）：谓词→field 单值 Record；本域再做
  // 白名单 + 字符串 + 非空收敛（等价旧手写叶子遍历）。
  const values = filterValues(predicate);
  const out: Partial<Record<FilterField, string>> = {};
  for (const field of FILTER_FIELDS) {
    const v = values[field];
    if (typeof v === "string" && v !== "") out[field] = v;
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

// —— 新建：表单值（展示形）在 run 内收敛为提交形（labels 逗号串→数组）——

export interface CreateUserForm {
  email: string;
  password: string;
  name: string;
  status: string;
  /** 逗号分隔展示形。 */
  labels: string;
}

export const usersResource = defineResource({
  name: "users",
  getId: (row: User) => row.id,
  scope: ["projectId"],
  collection: fetchUsers,
  // 存量键失效桥（§12.10-②）：命令 ok 后除 kit 键外失效存量 ["users"]
  // 宽前缀键（["users", id] 详情 / ["users", id, "sessions"] 会话全前缀
  // 族）——页面 onSettled 手动桥退役。
  legacyKeys: () => [["users"]],
  commands: {
    create: {
      run: async (form: CreateUserForm): Promise<CommandResult<User>> =>
        runCommandMapped(() =>
          createUser(
            {
              email: form.email,
              password: form.password,
              name: form.name,
              status: form.status,
              labels: form.labels
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean),
            },
            SKIP,
          ),
        ),
    },
    // 编辑输入自包含（diff 语义由页面提交处决定 name/email/email_verified 是否传）。
    update: {
      run: async (input: {
        id: string;
        status: string;
        name?: string;
        email?: string;
        email_verified?: boolean;
      }): Promise<CommandResult<User>> =>
        runCommandMapped(() =>
          updateUser(
            input.id,
            {
              status: input.status,
              name: input.name,
              email: input.email,
              email_verified: input.email_verified,
            },
            SKIP,
          ),
        ),
    },
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteUser(input.id, SKIP);
          return null;
        }),
      // 已删 → 幂等 ok：删除目标消失视为已达成（apiKeys remove 同款）
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
    resetPassword: {
      run: async (input: { id: string; password: string }): Promise<CommandResult<User>> =>
        runCommandMapped(() => updateUserPassword(input.id, input.password, SKIP)),
    },
  },
});

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createUserCommand = usersResource.commands.create;
export const updateUserCommand = usersResource.commands.update;
export const removeUserCommand = usersResource.commands.remove;
export const resetUserPasswordCommand = usersResource.commands.resetPassword;

// 裸 descriptor（不挂 resource——不涉 users 集合 kit 键失效；存量键桥在
// descriptor 自带 legacyKeys）：会话撤销；令牌签发（模拟登录，payload
// 一次性展示）。
export const revokeUserSessionDescriptor = {
  run: async (input: {
    userId: string;
    sessionId: string;
  }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await deleteUserSession(input.userId, input.sessionId, SKIP);
      return null;
    }),
  treatAs: { "not-found": { status: "ok" as const, payload: null } },
  // 会话不是集合形态：只桥存量 ["users", …] 前缀（含 sessions 键），不触
  // 发 kit 列表失效。
  legacyKeys: () => [["users"]],
};

export const issueUserTokenDescriptor = {
  run: async (input: { userId: string }): Promise<CommandResult<TokenBundle>> =>
    runCommandMapped(() => createUserToken(input.userId, SKIP)),
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；filters = 旧页真实传给服务端的四参（无其他服务端过滤）。
export const usersSearch = collectionQuerySchema({
  filters: FILTER_FIELDS.map((field) => ({ field })),
  defaultSize: 20,
});

// kit 列表键前缀（键形 [kind, 资源名, …]，见 ui-kit keys.ts；前缀匹配即
// 全窗口/全谓词失效）：useCommand 命令已按 resource 自动失效；此键留给
// 批量删除等命令式编排（非 useCommand 面）的手动失效。
export const usersCollectionKey = ["collection", "users"] as const;
