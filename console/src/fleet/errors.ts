import { isApiError, type ApiError, type CommandResult } from "@lynx-go/ui-kit/kernel";
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

// 失败通道的用户文案（toast 用）：与 fleet/values/states 的 ErrorState
// describe 同口径。结果通道（rejected/conflict）不走这里——归 Form 块结果盒。
export function describeApiError(error: ApiError): string {
  switch (error.kind) {
    case "network":
      return "网络不可达，请检查连接后重试";
    case "auth":
      return "会话已过期";
    case "forbidden":
      return "没有访问该资源的权限";
    case "not-found":
      return "资源不存在";
    case "decode":
      return `响应格式异常（${error.detail}）`;
    case "server":
      return `服务端错误 ${error.status}${error.message ? `：${error.message}` : ""}`;
  }
}

// gateway 错误体 {error:{code,message}} 的取码（结果两分映射用）。
export function gatewayErrorCode(err: unknown): string | undefined {
  return gatewayErrorBody(err)?.code;
}

// —— 结果两分映射的公共基件（§12.10-1 收敛：16 份逐字副本 → 单源）——
// 各域错误映射曾逐文件复制 gatewayMessage/isStatus/runCommandMapped，实证
// 14/16 语义全同、2 份静默漂移（丢 code）、仅 documents 有真 OCC 方言——
// 复制降级为单源 + documents 变体挂本层基件。

/** 网关错误体读取：axios 响应的 {status, error:{code,message}}，非 axios 形返回 undefined。 */
export function gatewayErrorBody(
  err: unknown,
): { status: number; code?: string; message?: string } | undefined {
  if (isAxiosError(err) && err.response) {
    const data = err.response.data as
      | { error?: { code?: unknown; message?: unknown } }
      | undefined;
    const e = data?.error;
    return {
      status: err.response.status,
      code: typeof e?.code === "string" && e.code !== "" ? e.code : undefined,
      message: typeof e?.message === "string" && e.message !== "" ? e.message : undefined,
    };
  }
  return undefined;
}

/** 结果分支文案：网关 error.message 优先，退回错误对象 message。 */
export function gatewayMessage(err: unknown): string {
  const body = gatewayErrorBody(err);
  if (body?.message) return body.message;
  return err instanceof Error ? err.message : String(err);
}

/**
 * 结果两分映射（标准形）：400→rejected{code,message}、409→conflict、
 * 其余 throw toApiError（404 幂等与否归 descriptor.treatAs 裁决）。
 * 域有额外方言（如 documents 的 OCC 域码表）时在域内自写变体，
 * 复用 gatewayErrorBody/gatewayMessage 基件。
 */
export async function runCommandMapped<Payload>(
  op: () => Promise<Payload>,
): Promise<CommandResult<Payload>> {
  try {
    return { status: "ok", payload: await op() };
  } catch (err) {
    const status = gatewayErrorBody(err)?.status;
    if (status === 400) {
      return { status: "rejected", code: gatewayErrorCode(err), message: gatewayMessage(err) };
    }
    if (status === 409) {
      return { status: "conflict", message: gatewayMessage(err) };
    }
    throw toApiError(err);
  }
}
