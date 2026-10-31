import {
  defineResource,
  type CollectionQuery,
  type CommandDescriptor,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { skipToast as SKIP } from "@/api/client";
import {
  deleteOAuthProvider,
  listOAuthProviders,
  upsertOAuthProvider,
  type OAuthProvider,
} from "@/api/oauthProviders";
import { runCommandMapped, toApiError } from "@/kit/errors";

// OAuth 提供方配置：写路径命令化（§12.9 阶段 2，结果两分 + __skipToast）+
// settings 面板分页列表资源化（子视图 kit 化批）。RPC 作用域来自
// X-Torchwood-Project header 而非请求参数——资源声明 scope ["projectId"]，
// ctx 缺参静默空段；projectId 不在命令 input（半在环境上下文）→ 命令 scope
// 由调用点显式传（half-scope 裁决，SKILL §12.12）。

async function fetchOAuthProviders(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<OAuthProvider>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listOAuthProviders({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const oauthProvidersResource = defineResource({
  name: "oauth-providers",
  getId: (row: OAuthProvider) => row.provider,
  scope: ["projectId"],
  collection: fetchOAuthProviders,
});

export type UpsertOAuthProviderInput = Parameters<typeof upsertOAuthProvider>[0];

export const upsertOAuthProviderCommand: CommandDescriptor<UpsertOAuthProviderInput, OAuthProvider> = {
  // 失效 = kit oauth-providers 键（子视图 kit 化批：存量 ["oauth-providers"]
  // 前缀桥退役——最后读者 settings 面板列表已换轨 useCollectionData）。
  invalidates: [{ resource: "oauth-providers" }],
  async run(input) {
    return runCommandMapped(() => upsertOAuthProvider(input, SKIP));
  },
};

export const removeOAuthProviderCommand: CommandDescriptor<{ provider: string }, null> = {
  invalidates: [{ resource: "oauth-providers" }],
  async run(input) {
    await runCommandMapped(async () => {
      await deleteOAuthProvider(input.provider, SKIP);
      return null;
    });
    return { status: "ok", payload: null };
  },
};
