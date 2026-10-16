import type { HttpTransport } from "../http.js";
import type { InvokeFunctionResponse } from "../types.js";

export interface InvokeFunctionInput {
  /** 函数入参：JSON object 字符串（≤32KB）。 */
  data?: string;
  /**
   * 可选客户端幂等键：网络超时重试防重复执行。(project, function, user, key)
   * 唯一去重，命中返回既有 invocation 原样（进行中返回 running）。
   * v4：deployment_id 已随部署面退役（调用目标恒为 endpoint target）。
   */
  idempotency_key?: string;
}

export class ClientFunctionsService {
  constructor(private readonly http: HttpTransport) {}

  /**
   * 同步调用一个 client_callable 函数（POST /v1/functions/{id}:invoke）。
   *
   * 错误形态（TorchwoodError）：
   *   - 403 PERMISSION_DENIED：函数未开启 client_callable；
   *   - 429 RESOURCE_EXHAUSTED：每用户限频超限（Reason =
   *     FUNCTIONS.INVOKE_QUOTA_EXCEEDED + RetryInfo 窗口结束时刻）或并发
   *     闸门排队超时；
   *   - 函数执行失败不是传输错误：HTTP 200 + status="failed"。
   */
  async invokeFunction(functionId: string, input?: InvokeFunctionInput): Promise<InvokeFunctionResponse> {
    return this.http.request<InvokeFunctionResponse>(
      "POST",
      `/v1/functions/${encodeURIComponent(functionId)}:invoke`,
      {
        body: {
          function_id: functionId,
          data: input?.data,
          idempotency_key: input?.idempotency_key,
        },
      }
    );
  }
}
