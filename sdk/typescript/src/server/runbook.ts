import type { HttpTransport } from "../http.js";
import type { Int64String } from "../types.js";

/**
 * Server API runbook 状态面（proto server.v1.RunbookService，docs/design/
 * runbook.md §2.2）：只记录"哪些 step 已应用"的事实，动作在 CLI 引擎侧。
 * 项目上下文来自凭证（API key 绑定项目）。
 */

export interface RunbookStepState {
  /** int64：网关 protojson 序列化为字符串。 */
  version: Int64String;
  /** step 文件名去序号后的 name 段（NNNNNN_name.yaml）。 */
  name: string;
  /** sha256(归一化后文件字节)，小写十六进制。 */
  checksum: string;
  applied_at?: string;
}

export interface RecordRunbookStepInput {
  version: number;
  name: string;
  checksum: string;
  /** CAS：缺省 = 断言无前序（首步）；显式值必须等于当前顶版。 */
  expect_prev_version?: number;
}

export class RunbookService {
  constructor(private readonly http: HttpTransport) {}

  /** getState 返回指定 runbook 的已应用 step 升序全集（空 = 未应用）。 */
  async getState(runbook: string): Promise<{ steps: RunbookStepState[] }> {
    return this.http.request<{ steps: RunbookStepState[] }>(
      "GET",
      `/v1/server/runbooks/${encodeURIComponent(runbook)}/steps`,
      { auth: "apiKey" }
    );
  }

  /** recordStep 记录一个已应用 step（CAS 冲突 → FAILED_PRECONDITION）。 */
  async recordStep(
    runbook: string,
    input: RecordRunbookStepInput
  ): Promise<{ current_version: Int64String }> {
    return this.http.request<{ current_version: Int64String }>(
      "POST",
      `/v1/server/runbooks/${encodeURIComponent(runbook)}/steps`,
      { auth: "apiKey", body: input }
    );
  }

  /** deleteStep 摘除一条 step（down 回退 / forgive）：仅允许删除当前顶版。 */
  async deleteStep(runbook: string, version: number): Promise<void> {
    await this.http.request<void>(
      "DELETE",
      `/v1/server/runbooks/${encodeURIComponent(runbook)}/steps/${version}`,
      { auth: "apiKey" }
    );
  }
}
