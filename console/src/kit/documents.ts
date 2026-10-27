import { skipToast as SKIP } from "@/api/client";
import { invalidateAfterCommand } from "@lynx-go/uikit-core";
import {
  type CollectionQuery,
  type CommandDescriptor,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import { isAxiosError } from "axios";
import {
  createDocument,
  deleteDocument,
  listDocuments,
  updateDocument,
  type Attribute,
  type Document,
} from "@/api/databases";
import { gatewayErrorBody, toApiError } from "@/kit/errors";

// fleet-ui OCC 切片（documents，design.md §12.5）：update/delete 必带 version
// （CAS）。通道方言：OCC 冲突 = HTTP 400 + FailedPrecondition + message 前缀
// DOCUMENT.VERSION_CONFLICT（HTTP 409 反而不是 OCC——是重复键/幂等进行中）；
// 网关错误体的 ErrorInfo/BadRequest details（域码 reason、current_version、
// field_violations）在 HTTP 面被丢弃，域码判别唯一可靠面 = message 前缀
// "DOMAIN.CODE:"。域码展示文案映射表在本层（消费方文案权）。

// —— 列表 fleet 化（§12.9-2）：文档列表换轨 collection（命令族保持工厂
// 形态——dbId/collId 闭包是既有消费面与 OCC 测试面，不搬进资源 commands；
// 失效走 descriptor 声明（§12.10-②）：kit documents 键 + 存量 ["documents"]
// 前缀键——页面 onSettled 手动桥与 list.refetch 退役）。——

export interface DocumentFormValues {
  /** 展示形字段值（string；__json = 无 attributes 集合的整体 JSON） */
  values: Record<string, string>;
  /** 数值字段原子增量（展示形 string，如 "1"、"-2"；仅更新面消费） */
  increments?: Record<string, string>;
  /** permissions 编辑器值（仅创建面消费） */
  permissions?: string[];
  /** OCC 版本（来自 GetDocument/List；仅更新/删除面消费） */
  version?: number;
}

// 域码 → 展示文案（结果通道呈现；失败通道仍走 describeApiError）。
const DOMAIN_MESSAGES: Record<string, string> = {
  "DOCUMENT.VERSION_CONFLICT": "The document was modified by someone else (version mismatch). Please refresh and retry",
  "DOCUMENT.VERSION_REQUIRED": "The document is missing OCC version info. Please refresh and retry",
  "DOCUMENT.VERSION_INVALID": "Invalid OCC version value. Please refresh and retry",
  "DOCUMENT.ALREADY_EXISTS": "Document ID already exists. Please use a different ID and retry",
  "DOCUMENT.NOT_FOUND": "Document not found (it may have been deleted)",
  "DOCUMENT.INVALID_ARGUMENT": "Invalid request parameters",
  "DOCUMENT.NO_FIELDS_TO_UPDATE": "No fields to update",
  "IDEMPOTENCY.IN_PROGRESS": "A request with the same idempotency key is in progress, please try again later",
};

/** message 前缀域码解析（"DOCUMENT.VERSION_CONFLICT: ..." → 域码）。 */
export function domainCodeOf(message: string | undefined): string | undefined {
  const m = /^([A-Z][A-Z_]*\.[A-Z_]+):/.exec(message ?? "");
  return m?.[1];
}

function messageOf(domainCode: string | undefined, fallback: string | undefined): string {
  if (domainCode && DOMAIN_MESSAGES[domainCode]) return DOMAIN_MESSAGES[domainCode];
  return fallback ?? "Request failed";
}

/**
 * 结果两分映射（documents 版，§12.5 裁决 2/3/7）：
 * 400 + VERSION_CONFLICT 前缀 → conflict（OCC 冲突 = kit conflict 分支的
 * torchwood 真身）；400 其他 → rejected{code=域码前缀}；409 → conflict
 * （重复键/幂等进行中）；update 404 → throw（失败通道）；其余 throw。
 */
async function runDocumentCommand<Payload>(
  op: () => Promise<Payload>
): Promise<CommandResult<Payload>> {
  try {
    return { status: "ok", payload: await op() };
  } catch (err) {
    if (isAxiosError(err) && err.response) {
      const status = err.response.status;
      const body = gatewayErrorBody(err);
      const domainCode = domainCodeOf(body?.message);
      if (status === 400 && domainCode === "DOCUMENT.VERSION_CONFLICT") {
        return { status: "conflict", message: messageOf(domainCode, body?.message) };
      }
      if (status === 400) {
        return {
          status: "rejected",
          code: domainCode ?? body?.code,
          message: messageOf(domainCode, body?.message),
        };
      }
      if (status === 409) {
        return { status: "conflict", message: messageOf(domainCode, body?.message) };
      }
      // 404 → toApiError（not-found ApiError）：幂等与否归 descriptor.treatAs
      // 裁决（delete 已删 → ok；update 404 = 文档被删 → 失败通道）
    }
    throw toApiError(err);
  }
}

// —— 提交收敛（展示形 → 提交形；客户端 fieldErrors 的唯一真实来源，§12.5-5）——

interface FieldIssues {
  message: string;
  fieldErrors?: Record<string, string>;
}

function parseFieldValue(type: string, raw: string): unknown {
  if (raw === "") return null;
  switch (type) {
    case "integer":
      return Number.parseInt(raw, 10);
    case "float":
      return Number.parseFloat(raw);
    case "boolean":
      return raw === "true";
    case "json":
      return JSON.parse(raw);
    default:
      return raw;
  }
}

/** 展示形字段值 → 提交形 data；违规落 fieldErrors（不抛——结果通道）。 */
function buildDocumentData(
  attributes: Attribute[],
  values: Record<string, string>
): { data?: Record<string, unknown>; issues?: FieldIssues } {
  if (attributes.length === 0) {
    try {
      return { data: JSON.parse(values.__json || "{}") as Record<string, unknown> };
    } catch (err) {
      return {
        issues: {
          message: "Invalid Data JSON format",
          fieldErrors: { __json: `JSON parse failed: ${err instanceof Error ? err.message : String(err)}` },
        },
      };
    }
  }
  const data: Record<string, unknown> = {};
  const fieldErrors: Record<string, string> = {};
  for (const attr of attributes) {
    const raw = values[attr.key];
    if (raw === undefined || raw === "") {
      if (attr.required) fieldErrors[attr.key] = "Required field";
      continue;
    }
    if (attr.type === "json") {
      try {
        data[attr.key] = JSON.parse(raw);
      } catch (err) {
        fieldErrors[attr.key] = `JSON parse failed: ${err instanceof Error ? err.message : String(err)}`;
      }
      continue;
    }
    data[attr.key] = parseFieldValue(attr.type, raw);
  }
  if (Object.keys(fieldErrors).length > 0) {
    return { issues: { message: "Some fields failed validation", fieldErrors } };
  }
  return { data };
}

/** 增量展示形 → 提交形；非整数落 fieldErrors。 */
function buildIncrement(
  attributes: Attribute[],
  increments: Record<string, string>
): { increment?: Record<string, number>; issues?: FieldIssues } {
  const increment: Record<string, number> = {};
  const fieldErrors: Record<string, string> = {};
  for (const attr of attributes) {
    const delta = increments[attr.key]?.trim();
    if (!delta) continue;
    const n = Number(delta);
    if (!Number.isInteger(n)) {
      fieldErrors[`${attr.key}Δ`] = "Increment must be an integer";
      continue;
    }
    if (n !== 0) increment[attr.key] = n;
  }
  if (Object.keys(fieldErrors).length > 0) {
    return { issues: { message: "Some fields failed validation", fieldErrors } };
  }
  return Object.keys(increment).length > 0 ? { increment } : {};
}

// —— descriptor 工厂（裸 CommandDescriptor，路径参数闭包——批 B editApiKeyDescriptor 先例）——
// 三工厂共用同一份失效声明（§12.10-②）：kit documents 键（selector 与命令
// ctx scope 相交——页面传全量嵌套 scope）+ 存量 ["documents"] 前缀键（详情
// ["documents", dbId, collId, docId] 仍消费）。

/** 裸 descriptor 的失效声明（kit documents 资源键 + 存量前缀桥）。 */
const DOCUMENTS_INVALIDATION = {
  invalidates: [{ resource: "documents" }],
  legacyKeys: () => [["documents"]],
} as const;

/** 创建：表单展示形在 run 内收敛（data/permissions）；重复键 409 → conflict。 */
export function createDocumentCommand(
  dbId: string,
  collId: string,
  attributes: Attribute[]
): CommandDescriptor<DocumentFormValues, Document> {
  return {
    ...DOCUMENTS_INVALIDATION,
    async run(input) {
      const built = buildDocumentData(attributes, input.values);
      if (built.issues) return { status: "rejected", ...built.issues };
      const payload = await runDocumentCommand(() =>
        createDocument(
          dbId,
          collId,
          {
            data: built.data ?? {},
            ...(input.permissions && input.permissions.length > 0
              ? { permissions: input.permissions }
              : {}),
          },
          SKIP
        )
      );
      return payload;
    },
  };
}

export interface UpdateDocumentInput extends DocumentFormValues {
  docId: string;
}

/**
 * 更新（OCC 主面）：version 前置守卫 = 客户端 rejected（非 throw——已知分支
 * 不占失败通道，§12.5-3）；OCC 冲突 → conflict（页面渲染「刷新重载」）。
 */
export function updateDocumentCommand(
  dbId: string,
  collId: string,
  attributes: Attribute[]
): CommandDescriptor<UpdateDocumentInput, Document> {
  return {
    ...DOCUMENTS_INVALIDATION,
    async run(input) {
      if (input.version === undefined || input.version <= 0) {
        return {
          status: "rejected",
          code: "DOCUMENT.VERSION_REQUIRED",
          message: DOMAIN_MESSAGES["DOCUMENT.VERSION_REQUIRED"]!,
        };
      }
      const version = input.version;
      const built = buildDocumentData(attributes, input.values);
      if (built.issues) return { status: "rejected", ...built.issues };
      const inc = buildIncrement(attributes, input.increments ?? {});
      if (inc.issues) return { status: "rejected", ...inc.issues };
      return runDocumentCommand(() =>
        updateDocument(
          dbId,
          collId,
          input.docId,
          {
            data: built.data ?? {},
            ...(inc.increment ? { increment: inc.increment } : {}),
            version,
          },
          SKIP
        )
      );
    },
  };
}

/** 删除（OCC）：version 守卫同上；404 → treatAs 幂等 ok（批 B 先例）。 */
export function removeDocumentCommand(
  dbId: string,
  collId: string
): CommandDescriptor<{ docId: string; version?: number }, null> {
  return {
    ...DOCUMENTS_INVALIDATION,
    treatAs: { "not-found": { status: "ok" as const, payload: null } },
    async run(input) {
      if (input.version === undefined || input.version <= 0) {
        return {
          status: "rejected",
          code: "DOCUMENT.VERSION_REQUIRED",
          message: DOMAIN_MESSAGES["DOCUMENT.VERSION_REQUIRED"]!,
        };
      }
      const version = input.version;
      const outcome = await runDocumentCommand(async () => {
        await deleteDocument(dbId, collId, input.docId, version, SKIP);
      });
      // 结果分支（conflict/rejected）原样透出——删除无表单容器，页面 toast 呈现
      return outcome.status === "ok" ? { status: "ok", payload: null } : outcome;
    },
  };
}

// —— 列表资源（§12.9-2）——

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页默认（useServerPaging 缺省）；listDocuments 无真实服务端过滤
// 参数（仅分页）→ 不设 filters/text，页内搜索是本地态投影不进 URL。
export const documentsSearch = collectionQuerySchema({ defaultSize: 20 });

async function fetchDocuments(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Document>> {
  // 嵌套 scope：dbId/collId 进 ctx（projectId 缺 = 会话未就绪），缺参静默空段。
  if (ctx.projectId == null || ctx.dbId == null || ctx.collId == null) {
    return { rows: [] };
  }
  try {
    const page = await listDocuments(ctx.dbId, ctx.collId, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const documentsResource = defineResource({
  name: "documents",
  getId: (row: Document) => row.id,
  scope: ["projectId", "dbId", "collId"],
  collection: fetchDocuments,
});

// —— 批量直调路径的失效（§12.10-②）：bulk 更新/删除无命令容器（Promise
// 直调 + 汇总 toast），页面经 invalidateAfterCommand 复用同一份失效声明
//（kit documents 键自动刷新 useCollection 列表 + 存量前缀键）。——

/** 失效声明载体（run 不被执行——仅供 invalidateAfterCommand 读取声明面）。 */
const BULK_INVALIDATION_DESCRIPTOR: CommandDescriptor<unknown, null> = {
  ...DOCUMENTS_INVALIDATION,
  run: async () => ({ status: "ok", payload: null }),
};

/** 直调批量写后的缓存失效；scope 传全量嵌套对象（与列表绑定同形，相交匹配）。 */
export function invalidateDocumentsAfterBulkWrite(
  queryClient: Parameters<typeof invalidateAfterCommand>[0],
  scope: ResourceScope
): void {
  invalidateAfterCommand(queryClient, BULK_INVALIDATION_DESCRIPTOR, scope);
}
