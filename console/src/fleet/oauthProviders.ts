import type { CommandDescriptor } from "@lynx-go/ui-kit/kernel";
import { skipToast as SKIP } from "@/api/client";
import { deleteOAuthProvider, upsertOAuthProvider, type OAuthProvider } from "@/api/oauthProviders";
import { runCommandMapped } from "@/fleet/errors";

// OAuth 提供方配置的写路径命令化（§12.9 阶段 2）：结果两分 + __skipToast；
// 失效桥在页面（存量 ["oauth-providers"] 键）。

export type UpsertOAuthProviderInput = Parameters<typeof upsertOAuthProvider>[0];

export const upsertOAuthProviderCommand: CommandDescriptor<UpsertOAuthProviderInput, OAuthProvider> = {
  async run(input) {
    return runCommandMapped(() => upsertOAuthProvider(input, SKIP));
  },
};

export const removeOAuthProviderCommand: CommandDescriptor<{ provider: string }, null> = {
  async run(input) {
    await runCommandMapped(async () => {
      await deleteOAuthProvider(input.provider, SKIP);
      return null;
    });
    return { status: "ok", payload: null };
  },
};
