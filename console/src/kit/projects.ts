import {
  defineResource,
  type CollectionQuery,
  type CommandResult,
  type CommandDescriptor,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  createInviteCode,
  createProject,
  deleteInviteCode,
  deleteProject,
  listProjects,
  updateOAuthRedirectAllowlist,
  updateProject,
  type InviteCode,
  type Project,
} from "@/api/projects";
import { runCommandMapped, toApiError } from "@/kit/errors";

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
  // 存量键失效桥（§12.10-②，平台级资源——legacyKeys 与 scope 声明无关）：
  // 命令 ok 后除 kit 键外失效存量 ["projects"] 宽前缀键（settings/详情页
  // 仍吃存量键）。
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
  // 存量键失效桥（§12.10-②）：["projects", id] 详情/["projects"] 全前缀族。
  legacyKeys: () => [["projects"]],
  async run(i) {
    const { id, ...patch } = i;
    return runCommandMapped(() => updateProject(id, patch, SKIP));
  },
};

export const removeProjectCommand: CommandDescriptor<{ id: string }, null> = {
  treatAs: { "not-found": { status: "ok" as const, payload: null } },
  legacyKeys: () => [["projects"]],
  async run(i) {
    await runCommandMapped(async () => {
      await deleteProject(i.id, SKIP);
      return null;
    });
    return { status: "ok", payload: null };
  },
};

// —— settings 其余写路径（OAuth 白名单/邀请码）——

export const updateOAuthAllowlistCommand: CommandDescriptor<
  { id: string; urls: string[] },
  Project
> = {
  // 白名单落在 project 实体上（PATCH 回 Project）——失效存量 ["projects"]。
  legacyKeys: () => [["projects"]],
  async run(i) {
    return runCommandMapped(() => updateOAuthRedirectAllowlist(i.id, i.urls, SKIP));
  },
};

export const createInviteCodeCommand: CommandDescriptor<
  { projectId: string; maxUses?: number; expireAt?: string },
  InviteCode
> = {
  legacyKeys: () => [["invite-codes"]],
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
  legacyKeys: () => [["invite-codes"]],
  async run(i) {
    await runCommandMapped(async () => {
      await deleteInviteCode(i.projectId, i.id, SKIP);
      return null;
    });
    return { status: "ok", payload: null };
  },
};
