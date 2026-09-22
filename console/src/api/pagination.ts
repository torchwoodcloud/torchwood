// Console 服务端列表的分页契约类型。
//
// 背景（2026-09-21 assets 列表只显示 25 条而 CLI 可见全部）：服务端各列表
// 端点有各自的默认 page_size（assets/billing/payments/subscriptions = 25，
// databases/documents = 50），请求不带 page_size 时只返回第一页 +
// meta.next_page_token。裁决：Console 列表页一律对接真实的服务端分页——
// 列表封装接受 { pageSize, pageToken } 并返回一页（Page<T>），翻页由页面经
// useServerPaging 的 token 栈驱动（先例：audit-logs）。裸 GET 第一页会被
// api-pagination-guard.test.ts 拦下。
//
// 终止契约：服务端仅在空页时停发 next_page_token（而不是"返回数 < 页大小"），
// has_next 以 token 是否存在判定；keyset 游标下 total 未知，UI 不显示总数。

// shared.v1.ListResponseMeta 的 JSON 投影（gateway UseProtoNames，snake_case）。
export interface ListMeta {
  meta?: { next_page_token?: string };
}

export interface Page<T> {
  rows: T[];
  nextPageToken?: string;
}

// 列表封装的分页入参：pageSize 必填（显式选择，杜绝无意识落在服务端默认页）。
export interface ListParams {
  pageSize: number;
  pageToken?: string;
}

// axios params 形参（gateway UseProtoNames，query 用 snake_case）。
export function pageQuery(params: ListParams) {
  return {
    page_size: params.pageSize,
    page_token: params.pageToken || undefined,
  };
}

// grpc-gateway 的 repeated query 字段要求 key=a&key=b 形式；axios 默认把数组
// 序列化成 key[]=a&key[]=b（实测 1.x），gateway 不识别 → 过滤参数静默失效。
// 携带数组参数的列表封装必须经 paramsSerializer 使用本函数。
export function serializeGatewayParams(params: Record<string, unknown>): string {
  const sp = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === "") continue;
    if (Array.isArray(value)) {
      for (const item of value) {
        if (item !== undefined && item !== null && item !== "") sp.append(key, String(item));
      }
    } else {
      sp.append(key, String(value));
    }
  }
  return sp.toString();
}

// UI 可选页大小；上限 100 = 服务端各列表端点的 clamp 上限（assets/billing
// maxListLimit、documents maxQueryLimit）。
export const PAGE_SIZE_OPTIONS = [20, 50, 100] as const;
