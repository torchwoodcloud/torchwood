import type { CommandDescriptor } from "@lynx-go/ui-kit/kernel";
import { skipToast as SKIP } from "@/api/client";
import { deleteOAuthProvider, upsertOAuthProvider, type OAuthProvider } from "@/api/oauthProviders";
import { runCommandMapped } from "@/fleet/errors";

// OAuth 提供方配置的写路径命令化（§12.9 阶段 2）：结果两分 + __skipToast。
// 裸 descriptor（配置非集合形态，不挂 resource——kit 键无谓失效可免）；
// 失效桥声明在 descriptor 自带 legacyKeys（§12.10-②）：ok 后失效存量
// ["oauth-providers"] 宽前缀键（settings 面板列表仍吃存量键）——页面
// 手动桥退役。

export type UpsertOAuthProviderInput = Parameters<typeof upsertOAuthProvider>[0];

export const upsertOAuthProviderCommand: CommandDescriptor<UpsertOAuthProviderInput, OAuthProvider> = {
  legacyKeys: () => [["oauth-providers"]],
  async run(input) {
    return runCommandMapped(() => upsertOAuthProvider(input, SKIP));
  },
};

export const removeOAuthProviderCommand: CommandDescriptor<{ provider: string }, null> = {
  legacyKeys: () => [["oauth-providers"]],
  async run(input) {
    await runCommandMapped(async () => {
      await deleteOAuthProvider(input.provider, SKIP);
      return null;
    });
    return { status: "ok", payload: null };
  },
};
