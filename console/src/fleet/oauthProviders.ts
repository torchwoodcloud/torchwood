import type { CommandDescriptor, CommandResult } from "@lynx-go/ui-kit/kernel";
import { deleteOAuthProvider, upsertOAuthProvider, type OAuthProvider } from "@/api/oauthProviders";
import { toApiError } from "@/fleet/errors";

// OAuth 提供方配置的写路径命令化（§12.9 阶段 2）：结果两分 + __skipToast；
// 失效桥在页面（存量 ["oauth-providers"] 键）。

interface GatewayErrorBody {
  error?: { message?: string };
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

async function runCommandMapped<Payload>(op: () => Promise<Payload>): Promise<CommandResult<Payload>> {
  try {
    return { status: "ok", payload: await op() };
  } catch (err) {
    if (isStatus(err, 400)) return { status: "rejected", message: gatewayMessage(err) };
    if (isStatus(err, 409)) return { status: "conflict", message: gatewayMessage(err) };
    throw toApiError(err);
  }
}

const SKIP = { __skipToast: true } as const;

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
