import { api } from "./client";

// 字段为 snake_case：grpc-gateway 以 json_names_for_fields=false 序列化
// （与 users.ts 等现有封装一致）。
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

export interface ListAuditLogsParams {
  page_size?: number;
  page_token?: string;
  action?: string;
  status?: string;
  actor_id?: string;
  resource_id?: string;
  created_after?: string;
  created_before?: string;
  include_platform?: boolean;
  all_projects?: boolean;
}

export interface ListAuditLogsResponse {
  audit_logs: AuditLog[];
  meta?: {
    total_count?: number;
    page_size?: number;
    next_page_token?: string;
    prev_page_token?: string;
  };
}

// listAuditLogs 查询审计日志（项目上下文经 client 拦截器注入
// X-Torchwood-Project；include_platform/all_projects 仅平台 admin 可用，
// 服务端 PermissionDenied 兜底）。
export async function listAuditLogs(
  params: ListAuditLogsParams
): Promise<ListAuditLogsResponse> {
  const clean: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "" && v !== false) {
      clean[k] = v;
    }
  }
  const res = await api.get<ListAuditLogsResponse>("/server/audit-logs", {
    params: clean,
  });
  return {
    audit_logs: res.data.audit_logs ?? [],
    meta: res.data.meta,
  };
}
