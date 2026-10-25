import {
  defineResource,
  type CollectionQuery,
  type CommandResult,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import {
  createGroup,
  createMembership,
  deleteGroup,
  deleteMembership,
  listGroups,
  updateGroupPrefs,
  updateMembership,
  updateMembershipStatus,
  type Group,
  type Membership,
} from "@/api/groups";
import { gatewayErrorCode, toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）+ 命令族切片（§12.3，apiKeys 同款）：groups 列表
// 换 useCollection——游标/页大小进 URL（旧页无服务端过滤——不设 filters）；
// 数据面复用 axios 管线（刷新/项目头），失败在适配器收敛为 ApiError
//（fleet/errors.ts）。写路径换 useCommand：结果两分映射——400→rejected、
// 409→conflict、其余 throw ApiError；命令调用一律 __skipToast（toast 单源 =
// onSettled）；kit 列表键自动失效（descriptor 挂 resource），存量键
//（["groups", …]/["memberships", …]——详情页仍吃存量键）由页面 onSettled
// 手动桥接。

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
  commands: {
    create: {
      run: async (input: { name: string }): Promise<CommandResult<Group>> =>
        runCommandMapped(() => createGroup({ name: input.name }, SKIP)),
    },
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteGroup(input.id, SKIP);
          return null;
        }),
      // 已删 → 幂等 ok：删除目标消失视为已达成（apiKeys remove 同款）
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
    // 偏好整体替换（PUT）；输入自包含。
    updatePrefs: {
      run: async (input: {
        id: string;
        prefs: Record<string, unknown>;
      }): Promise<CommandResult<Record<string, unknown>>> =>
        runCommandMapped(() => updateGroupPrefs(input.id, input.prefs, SKIP)),
    },
    // 成员增删/角色/状态：groupId 进输入（自包含，详情页无实体快照依赖）。
    addMember: {
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

// 命令句柄提升（demo 同款 commands! 模式——Resource.commands 可选类型面）。
export const createGroupCommand = groupsResource.commands!.create;
export const removeGroupCommand = groupsResource.commands!.remove;
export const updateGroupPrefsCommand = groupsResource.commands!.updatePrefs;
export const addGroupMemberCommand = groupsResource.commands!.addMember;
export const setGroupMemberRoleCommand = groupsResource.commands!.setMemberRole;
export const setGroupMemberStatusCommand = groupsResource.commands!.setMemberStatus;
export const removeGroupMemberCommand = groupsResource.commands!.removeMember;

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；无服务端过滤参数——不设 filters。
export const groupsSearch = collectionQuerySchema({ defaultSize: 20 });

// kit 列表键前缀（键形 [kind, 资源名, …]，见 ui-kit keys.ts；前缀匹配即
// 全窗口/全谓词失效）：useCommand 命令已按 resource 自动失效；此键留给
// 批量删除等命令式编排（非 useCommand 面）的手动失效。
export const groupsCollectionKey = ["collection", "groups"] as const;
