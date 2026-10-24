import { isApiError, type ApiError } from "@lynx-go/ui-kit/kernel";
import { isAxiosError } from "axios";

// torchwood axios 管线 → kit ApiError 封闭集的收敛缝（§12.2 裁决：axios
// 复用、kit http 收敛后置——资源适配器抛出的失败必须是 ApiError，绑定的
// 重试分类与 ErrorState 分诊才成立）。401 到达调用方 = 拦截器刷新后仍死
// 会话；gateway 错误体形如 {error: {code, message}}。
export function toApiError(err: unknown): ApiError {
  if (isApiError(err)) return err;
  if (isAxiosError(err)) {
    if (err.response) {
      const status = err.response.status;
      const body = err.response.data as { error?: { message?: unknown } } | undefined;
      const message =
        typeof body?.error?.message === "string" ? body.error.message : err.message;
      if (status === 401) return { kind: "auth" };
      if (status === 403) return { kind: "forbidden" };
      if (status === 404) return { kind: "not-found" };
      return { kind: "server", status, message };
    }
    return { kind: "network", cause: err };
  }
  return { kind: "server", status: 0, message: err instanceof Error ? err.message : String(err) };
}
