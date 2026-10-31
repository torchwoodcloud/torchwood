import {
  defineResource,
  type CollectionQuery,
  type CommandResult,
  type CommandDescriptor,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  createInviteCode,
  createProject,
  deleteInviteCode,
  deleteProject,
  getProject,
  listInviteCodes,
  listProjects,
  updateOAuthRedirectAllowlist,
  updateProject,
  type InviteCode,
  type Project,
} from "@/api/projects";
import { recordOrNull, runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：projects 是平台级资源（GET /server/projects，
// 旧页不随项目作用域）——defineResource 不设 scope，直接请求；游标/页大小
// 进 URL（旧页无服务端过滤——不设 filters）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（kit/errors.ts）。

// 平台级域无 scope 约定：ctx 恒为 {}，签名只收 q。
async function fetchProjects(q: CollectionQuery): Promise<Segment<Project>> {
  try {
    const page = await listProjects({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const projectsResource = defineResource({
  name: "projects",
  getId: (row: Project) => row.id,
  collection: fetchProjects,
  // 详情读（ProjectDetailPage / ProjectSettingsPage，子视图 kit 化批）：
  // 404 → null 空态直出（recordOrNull 手写方言，不引 restRecord）。
  record: (id) => recordOrNull(() => getProject(id)) as Promise<Project>,
  // 存量键失效桥（§12.10-②）保留：壳层读者仍在——ProjectBootstrap /
  // ProjectSelector 读 ["projects"] 列表（不在子视图 kit 化批范围）；详情/
  // 设置页详情读已换轨 kit record 键（本资源命令 ok 的缺省同资源失效命中）。
  legacyKeys: () => [["projects"]],
  commands: {
    // 新建项目（实体集合变更）：description 空 = 不发送（run 内收敛）。
    create: {
      run: async (input: {
        id: string;
        name: string;
        description?: string;
      }): Promise<CommandResult<Project>> =>
        runCommandMapped(() =>
          createProject(
            { id: input.id, name: input.name, description: input.description || undefined },
            SKIP
          )
        ),
    },
  },
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；无服务端过滤参数——不设 filters。
export const projectsSearch = collectionQuerySchema({ defaultSize: 20 });

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createProjectCommand = projectsResource.commands.create;

// —— 写路径命令化（§12.9 阶段 2）：settings 页的更新/删除 ——
//（裸 descriptor：不挂 resource——kit 集合键无谓失效可免；失效 = 自带
// legacyKeys 宽前缀声明，§12.10-②。）

export interface UpdateProjectInput {
  id: string;
  name?: string;
  description?: string;
  registration_policy?: string;
}

// 输入自包含（id 在入参里）——直接是常量 descriptor，无需工厂。
export const updateProjectCommand: CommandDescriptor<UpdateProjectInput, Project> = {
  // 双通道失效：invalidates 打 kit projects 键（详情/设置页 record 读换轨后
  // 的承接——旧 ["projects"] 宽前缀不再命中 kit 键形，缺了它保存后受控
  // Select/DetailGrid 会回显旧值）；legacyKeys 仍打壳层 ["projects"] 列表
  //（ProjectBootstrap / ProjectSelector 仍在读，保留）。
  invalidates: [{ resource: "projects" }],
  legacyKeys: () => [["projects"]],
  async run(i) {
    const { id, ...patch } = i;
    return runCommandMapped(() => updateProject(id, patch, SKIP));
  },
};

export const removeProjectCommand: CommandDescriptor<{ id: string }, null> = {
  treatAs: { "not-found": { status: "ok" as const, payload: null } },
  // 双通道失效同 update（kit 键 + 壳层宽前缀）。
  invalidates: [{ resource: "projects" }],
  legacyKeys: () => [["projects"]],
  async run(i) {
    await runCommandMapped(async () => {
      await deleteProject(i.id, SKIP);
      return null;
    });
    return { status: "ok", payload: null };
  },
};

// —— invite-codes（settings Auth 面内嵌分页列表，子视图 kit 化批）：scope
// ["projectId"]，ctx 缺参静默空段（与 collections/documents 同方言）；伴读
// 形由页面 useCollectionData 消费（useServerPaging 游标进 window）。——

async function fetchInviteCodes(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<InviteCode>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listInviteCodes(ctx.projectId, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const inviteCodesResource = defineResource({
  name: "invite-codes",
  getId: (row: InviteCode) => row.id,
  scope: ["projectId"],
  collection: fetchInviteCodes,
});

// —— settings 其余写路径（OAuth 白名单/邀请码）——

export const updateOAuthAllowlistCommand: CommandDescriptor<
  { id: string; urls: string[] },
  Project
> = {
  // 白名单落在 project 实体上（PATCH 回 Project）——双通道失效同 update
  //（kit projects 键 + 壳层 ["projects"] 宽前缀）。
  invalidates: [{ resource: "projects" }],
  legacyKeys: () => [["projects"]],
  async run(i) {
    return runCommandMapped(() => updateOAuthRedirectAllowlist(i.id, i.urls, SKIP));
  },
};

export const createInviteCodeCommand: CommandDescriptor<
  { projectId: string; maxUses?: number; expireAt?: string },
  InviteCode
> = {
  // 失效 = kit invite-codes 键（子视图 kit 化批：存量 ["invite-codes"] 前缀桥
  // 退役——最后读者 settings 分页列表已换轨 useCollectionData）；projectId 在
  // input → scopeOf 派生命令 scope（M2 同源：既喂 run ctx 又喂失效匹配）。
  scopeOf: (i) => ({ projectId: i.projectId }),
  invalidates: [{ resource: "invite-codes" }],
  async run(i) {
    return runCommandMapped(() =>
      createInviteCode(
        i.projectId,
        {
          ...(i.maxUses !== undefined ? { max_uses: i.maxUses } : {}),
          ...(i.expireAt ? { expire_at: new Date(i.expireAt).toISOString() } : {}),
        },
        SKIP
      )
    );
  },
};

export const removeInviteCodeCommand: CommandDescriptor<
  { projectId: string; id: string },
  null
> = {
  // 失效/命令 scope 同 create（scopeOf 派生）。
  scopeOf: (i) => ({ projectId: i.projectId }),
  invalidates: [{ resource: "invite-codes" }],
  async run(i) {
    await runCommandMapped(async () => {
      await deleteInviteCode(i.projectId, i.id, SKIP);
      return null;
    });
    return { status: "ok", payload: null };
  },
};
