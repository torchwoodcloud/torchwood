import {
  defineResource,
  type CollectionQuery,
  type CommandResult,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  createGroup,
  createMembership,
  deleteGroup,
  deleteMembership,
  getGroup,
  getGroupPrefs,
  listGroups,
  listMemberships,
  updateGroupPrefs,
  updateMembership,
  updateMembershipStatus,
  type Group,
  type Membership,
} from "@/api/groups";
import { recordOrNull, runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）+ 命令族切片（§12.3，apiKeys 同款）：groups 列表
// 换 useCollection——游标/页大小进 URL（旧页无服务端过滤——不设 filters）；
// 数据面复用 axios 管线（刷新/项目头），失败在适配器收敛为 ApiError
//（kit/errors.ts）。写路径换 useCommand：结果两分映射——400→rejected、
// 409→conflict、其余 throw ApiError；命令调用一律 __skipToast（toast 单源 =
// onSettled）；失效 = descriptor 声明（kit 键自动失效；跨资源连带显式
// invalidates——成员命令族打 groups + memberships，prefs 写打 groups +
// group-prefs）。存量 ["groups"]/["memberships"] 前缀桥已随子视图 kit 化
// 退役（详情/偏好/成员读全部换轨 kit record/collection 键，读者清零）。

async function fetchGroups(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Group>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listGroups({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const groupsResource = defineResource({
  name: "groups",
  getId: (row: Group) => row.id,
  scope: ["projectId"],
  collection: fetchGroups,
  // 详情读（GroupDetailPage 子视图，子视图 kit 化批）：404 → null 空态直出
  //（recordOrNull 手写方言，不引 restRecord）；同资源命令缺省失效全 kind
  // 命中本键（create/remove/updatePrefs 后详情自刷新）。
  record: (id) => recordOrNull(() => getGroup(id)) as Promise<Group>,
  // 存量键失效桥已退役（子视图 kit 化批）：["groups"]/["memberships"] 前缀
  // 读者清零（详情/偏好/成员读换轨各自资源 kit 键；跨资源连带改由下方
  // 命令的显式 invalidates 声明承接）。
  commands: {
    create: {
      // 建删变更组的成员集合存在性：连带失效 memberships 缓存（旧资源级
      // legacyKeys ["memberships"] 宽桥对建删也生效——同 id 重建防死缓存，
      // 宽度与旧前缀桥一致；groupId 命令侧不可知 → allScopes 逃逸，与
      // databases 建删 → collections 级联同款裁决）。
      invalidates: [{ resource: "groups" }, { resource: "memberships", allScopes: true }],
      run: async (input: { name: string }): Promise<CommandResult<Group>> =>
        runCommandMapped(() => createGroup({ name: input.name }, SKIP)),
    },
    remove: {
      invalidates: [{ resource: "groups" }, { resource: "memberships", allScopes: true }],
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteGroup(input.id, SKIP);
          return null;
        }),
      // 已删 → 幂等 ok：删除目标消失视为已达成（apiKeys remove 同款）
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
    // 偏好整体替换（PUT）；输入自包含。失效显式声明（声明优先 = 整体替换
    // 缺省，同资源 groups 须带上）：groups（详情 record kit 键——旧宽桥打
    // 详情，保真）+ group-prefs（偏好子视图详情，本切片换绑出的新资源）。
    // 页面侧须显式传全量嵌套 scope（groupId 进命令 scope，group-prefs 键
    // 投影轴才命中）。
    updatePrefs: {
      invalidates: [{ resource: "groups" }, { resource: "group-prefs" }],
      run: async (input: {
        id: string;
        prefs: Record<string, unknown>;
      }): Promise<CommandResult<Record<string, unknown>>> =>
        runCommandMapped(() => updateGroupPrefs(input.id, input.prefs, SKIP)),
    },
    // 成员增删/角色/状态：groupId 进输入（自包含，详情页无实体快照依赖）。
    // 失效 = groups（详情 record 的 Members 计数）+ memberships（成员列表
    // collection）——旧 legacyKeys 宽前缀桥的 kit 键承接（§12.10-②）；页面
    // 侧须显式传全量嵌套 scope（memberships 键投影轴含 groupId）。
    addMember: {
      invalidates: [{ resource: "groups" }, { resource: "memberships" }],
      run: async (input: {
        groupId: string;
        email: string;
        name: string;
        roles: string[];
        status: string;
      }): Promise<CommandResult<Membership>> =>
        runCommandMapped(() =>
          createMembership(
            input.groupId,
            {
              email: input.email,
              name: input.name,
              roles: input.roles,
              status: input.status,
            },
            SKIP,
          ),
        ),
    },
    setMemberRole: {
      invalidates: [{ resource: "groups" }, { resource: "memberships" }],
      run: async (input: {
        groupId: string;
        membershipId: string;
        roles: string[];
      }): Promise<CommandResult<Membership>> =>
        runCommandMapped(() =>
          updateMembership(
            input.groupId,
            input.membershipId,
            { roles: input.roles },
            SKIP,
          ),
        ),
    },
    setMemberStatus: {
      invalidates: [{ resource: "groups" }, { resource: "memberships" }],
      run: async (input: {
        groupId: string;
        membershipId: string;
        status: string;
      }): Promise<CommandResult<Membership>> =>
        runCommandMapped(() =>
          updateMembershipStatus(
            input.groupId,
            input.membershipId,
            input.status,
            SKIP,
          ),
        ),
    },
    removeMember: {
      invalidates: [{ resource: "groups" }, { resource: "memberships" }],
      run: async (input: {
        groupId: string;
        membershipId: string;
      }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteMembership(input.groupId, input.membershipId, SKIP);
          return null;
        }),
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
  },
});

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createGroupCommand = groupsResource.commands.create;
export const removeGroupCommand = groupsResource.commands.remove;
export const updateGroupPrefsCommand = groupsResource.commands.updatePrefs;
export const addGroupMemberCommand = groupsResource.commands.addMember;
export const setGroupMemberRoleCommand = groupsResource.commands.setMemberRole;
export const setGroupMemberStatusCommand = groupsResource.commands.setMemberStatus;
export const removeGroupMemberCommand = groupsResource.commands.removeMember;

// —— group-prefs（GroupPrefsCard 子视图详情；嵌套 scope 加 groupId）——
// 仅 record 槽（无集合形态）；写路径挂 groupsResource.updatePrefs（跨资源
// invalidates 声明见上）。

export const groupPrefsResource = defineResource({
  name: "group-prefs",
  scope: ["projectId", "groupId"],
  // 详情读（GroupPrefsCard，子视图 kit 化批）：无 404→null 空态分支——卡片
  // 对 !prefs 无消费面（GET 缺省回 {}，`prefs !== undefined` 分支直出 "{}"
  // 文本域），404 经 toApiError 如实上抛（recordOrNull 的 404→null 会被该
  // 分支接住把 "null" 写进 JSON 文本域，行为不保真）。ctx.groupId 由
  // scopeReady 就绪门保证（查询未就绪即停用），! 仅类型收窄；记录 id 与之
  // 同值（键投影轴用 scope 声明）。
  record: async (_id, ctx) => {
    try {
      return await getGroupPrefs(ctx.groupId!);
    } catch (err) {
      throw toApiError(err);
    }
  },
});

// —— memberships（GroupDetailPage 内嵌成员列表；嵌套 scope 加 groupId，
// ctx 缺参静默空段）——

async function fetchMemberships(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Membership>> {
  if (ctx.projectId == null || ctx.groupId == null) return { rows: [] };
  try {
    const page = await listMemberships(ctx.groupId, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const membershipsResource = defineResource({
  name: "memberships",
  getId: (row: Membership) => row.id,
  scope: ["projectId", "groupId"],
  collection: fetchMemberships,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；无服务端过滤参数——不设 filters。
export const groupsSearch = collectionQuerySchema({ defaultSize: 20 });

// kit 列表键前缀（键形 [kind, 资源名, …]，见 ui-kit keys.ts；前缀匹配即
// 全窗口/全谓词失效）：useCommand 命令已按 resource 自动失效；此键留给
// 批量删除等命令式编排（非 useCommand 面）的手动失效。
export const groupsCollectionKey = ["collection", "groups"] as const;
