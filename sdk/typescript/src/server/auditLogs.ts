import type { HttpTransport } from "../http.js";

/** 审计日志行（写入侧为 gRPC 审计拦截器，此处只读）。 */
export interface AuditLog {
  id: string;
  project_id?: string;
  actor_id?: string;
  actor_kind?: string;
  action: string;
  status: string;
  resource_id?: string;
  ip?: string;
  user_agent?: string;
  metadata?: Record<string, unknown>;
  created_at?: string;
}

/**
 * 审计日志过滤（exact 匹配，时间范围为闭区间 RFC3339）。项目上下文来自
 * 凭证（API key 绑定项目）；include_platform / all_projects 仅平台 admin
 * 可用（服务端 PermissionDenied 兜底）。
 */
export interface AuditLogsListParams {
  page_size?: number;
  page_token?: string;
  actor_id?: string;
  actor_kind?: string;
  action?: string;
  status?: string;
  resource_id?: string;
  created_after?: string;
  created_before?: string;
  include_platform?: boolean;
  all_projects?: boolean;
}

export interface AuditLogsListResult {
  audit_logs: AuditLog[];
  meta?: {
    total_count?: number;
    page_size?: number;
    next_page_token?: string;
    prev_page_token?: string;
  };
}

export class AuditLogsService {
  constructor(private readonly http: HttpTransport) {}

  async list(params?: AuditLogsListParams): Promise<AuditLog[]> {
    const res = await this.listWithMeta(params);
    return res.audit_logs;
  }

  /** 同 list，但携带分页 meta（total_count / next_page_token / prev_page_token）。 */
  async listWithMeta(params?: AuditLogsListParams): Promise<AuditLogsListResult> {
    const query: Record<string, string | number | boolean | undefined> = {
      page_size: params?.page_size,
      page_token: params?.page_token,
      actor_id: params?.actor_id,
      actor_kind: params?.actor_kind,
      action: params?.action,
      status: params?.status,
      resource_id: params?.resource_id,
      created_after: params?.created_after,
      created_before: params?.created_before,
      include_platform: params?.include_platform,
      all_projects: params?.all_projects,
    };
    for (const k of Object.keys(query)) {
      if (query[k] === undefined || query[k] === "") delete query[k];
    }
    const res = await this.http.request<AuditLogsListResult>("GET", "/v1/server/audit-logs", {
      auth: "apiKey",
      query,
    });
    return { audit_logs: res.audit_logs ?? [], meta: res.meta };
  }
}
