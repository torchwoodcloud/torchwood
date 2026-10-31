import {
  defineResource,
  type CollectionQuery,
  type CommandResult,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema, filterValues } from "@lynx-go/uikit-core/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  createUser,
  createUserToken,
  deleteUser,
  deleteUserSession,
  getUser,
  listUserSessions,
  listUsers,
  updateUser,
  updateUserPassword,
  type TokenBundle,
  type User,
  type UserSession,
} from "@/api/users";
import { recordOrNull, runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）+ 命令族切片（§12.3，apiKeys 同款）：users 列表
// 换 useCollection——游标/页大小/服务端过滤（ParseUserList 白名单 DSL 的
// 四参）进 URL；数据面复用 axios 管线（刷新/项目头），失败在适配器收敛为
// ApiError（kit/errors.ts）。写路径换 useCommand：结果两分映射——
// 400→rejected、409→conflict、其余 throw ApiError；命令调用一律
// __skipToast（toast 单源 = onSettled）；失效 = descriptor 声明（kit 键自动
// 失效；重置密码连带 user-sessions 显式声明）。存量 ["users"] 前缀桥已随
// 子视图 kit 化退役（详情/编辑/会话读全部换轨 kit record/collection 键，
// 读者清零）。会话撤销/令牌签发不涉 users 集合语义 → 裸 descriptor（不挂
// resource，不触发无谓的列表失效；失效 = kit user-sessions 键显式声明）。

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
  // 详情读（UserDetailPage / UserEditPage 子视图，子视图 kit 化批）：404 →
  // null 空态直出（recordOrNull 手写方言，不引 restRecord）；同资源命令
  // 缺省失效全 kind 命中本键（update/remove/resetPassword 后详情自刷新）。
  record: (id) => recordOrNull(() => getUser(id)) as Promise<User>,
  // 存量键失效桥已退役（子视图 kit 化批）：["users"] 前缀读者清零（详情/
  // 编辑换轨 kit record 键、会话列表换轨 user-sessions collection 键）。
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
    // 重置密码 = 服务端全量吊销该用户会话（页面 toast 同文案）：连带失效
    // user-sessions 集合（旧 legacyKeys 宽桥打 ["users", id, "sessions"] 的
    // kit 键承接，行为保真）；声明优先 = 整体替换缺省，同资源 users 显式
    // 带上。页面侧须显式传全量 scope（user-sessions 键投影轴含 userId）。
    resetPassword: {
      invalidates: [{ resource: "users" }, { resource: "user-sessions" }],
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

// 裸 descriptor（不挂 resource——撤销/签发不涉 users 集合 kit 键失效）：
// 会话撤销；令牌签发（模拟登录，payload 一次性展示）。二者变更的都是
// user-sessions 集合（撤销删会话、签发建会话）而非 user 实体 → 失效声明 =
// kit user-sessions 键（撤销旧走 ["users"] 宽桥连详情键一并打过——过宽，
// 收窄；签发旧无失效声明，会话列表不自动刷新——本切片补齐）。页面侧须
// 显式传全量 scope（user-sessions 键投影轴含 userId；裸 descriptor 不采纳
// Provider context）。
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
  invalidates: [{ resource: "user-sessions" }],
};

export const issueUserTokenDescriptor = {
  invalidates: [{ resource: "user-sessions" }],
  run: async (input: { userId: string }): Promise<CommandResult<TokenBundle>> =>
    runCommandMapped(() => createUserToken(input.userId, SKIP)),
};

// —— user-sessions（SessionsCard 内嵌会话列表；嵌套 scope 加 userId）——

// 整取方言：listUserSessions 无分页参数——窗口忽略，全量行单段直出。
async function fetchUserSessions(
  _q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<UserSession>> {
  if (ctx.projectId == null || ctx.userId == null) return { rows: [] };
  try {
    return { rows: await listUserSessions(ctx.userId) };
  } catch (err) {
    throw toApiError(err);
  }
}

export const userSessionsResource = defineResource({
  name: "user-sessions",
  getId: (row: UserSession) => row.id,
  scope: ["projectId", "userId"],
  collection: fetchUserSessions,
});

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
