import type { CommandDescriptor, CommandResult } from "@lynx-go/ui-kit/kernel";
import { isAxiosError } from "axios";
import {
  createDocument,
  deleteDocument,
  updateDocument,
  type Attribute,
  type Document,
} from "@/api/databases";
import { toApiError } from "@/fleet/errors";

// fleet-ui OCC 切片（documents，design.md §12.5）：update/delete 必带 version
// （CAS）。通道方言：OCC 冲突 = HTTP 400 + FailedPrecondition + message 前缀
// DOCUMENT.VERSION_CONFLICT（HTTP 409 反而不是 OCC——是重复键/幂等进行中）；
// 网关错误体的 ErrorInfo/BadRequest details（域码 reason、current_version、
// field_violations）在 HTTP 面被丢弃，域码判别唯一可靠面 = message 前缀
// "DOMAIN.CODE:"。域码中文映射表在本层（消费方文案权）。

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

// 域码 → 中文文案（结果通道呈现；失败通道仍走 describeApiError）。
const DOMAIN_MESSAGES: Record<string, string> = {
  "DOCUMENT.VERSION_CONFLICT": "文档已被其他修改抢先提交（版本不匹配），请刷新后重试",
  "DOCUMENT.VERSION_REQUIRED": "文档缺少 OCC 版本信息，请刷新后重试",
  "DOCUMENT.VERSION_INVALID": "OCC 版本值非法，请刷新后重试",
  "DOCUMENT.ALREADY_EXISTS": "Document ID 已存在，请更换 ID 后重试",
  "DOCUMENT.NOT_FOUND": "文档不存在（可能已被删除）",
  "DOCUMENT.INVALID_ARGUMENT": "请求参数无效",
  "DOCUMENT.NO_FIELDS_TO_UPDATE": "没有可更新的字段变更",
  "IDEMPOTENCY.IN_PROGRESS": "同幂等键的请求仍在进行中，请稍后重试",
};

/** message 前缀域码解析（"DOCUMENT.VERSION_CONFLICT: ..." → 域码）。 */
export function domainCodeOf(message: string | undefined): string | undefined {
  const m = /^([A-Z][A-Z_]*\.[A-Z_]+):/.exec(message ?? "");
  return m?.[1];
}

function gatewayBody(err: unknown): { code?: string; message?: string } | undefined {
  if (isAxiosError(err)) {
    const data = err.response?.data as { error?: { code?: unknown; message?: unknown } } | undefined;
    const e = data?.error;
    if (e && typeof e === "object") {
      return {
        code: typeof e.code === "string" ? e.code : undefined,
        message: typeof e.message === "string" ? e.message : undefined,
      };
    }
  }
  return undefined;
}

function messageOf(domainCode: string | undefined, fallback: string | undefined): string {
  if (domainCode && DOMAIN_MESSAGES[domainCode]) return DOMAIN_MESSAGES[domainCode];
  return fallback ?? "请求失败";
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
      const body = gatewayBody(err);
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
          message: "Data JSON 格式无效",
          fieldErrors: { __json: `JSON 解析失败：${err instanceof Error ? err.message : String(err)}` },
        },
      };
    }
  }
  const data: Record<string, unknown> = {};
  const fieldErrors: Record<string, string> = {};
  for (const attr of attributes) {
    const raw = values[attr.key];
    if (raw === undefined || raw === "") {
      if (attr.required) fieldErrors[attr.key] = "必填字段";
      continue;
    }
    if (attr.type === "json") {
      try {
        data[attr.key] = JSON.parse(raw);
      } catch (err) {
        fieldErrors[attr.key] = `JSON 解析失败：${err instanceof Error ? err.message : String(err)}`;
      }
      continue;
    }
    data[attr.key] = parseFieldValue(attr.type, raw);
  }
  if (Object.keys(fieldErrors).length > 0) {
    return { issues: { message: "部分字段未通过校验", fieldErrors } };
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
      fieldErrors[`${attr.key}Δ`] = "增量必须是整数";
      continue;
    }
    if (n !== 0) increment[attr.key] = n;
  }
  if (Object.keys(fieldErrors).length > 0) {
    return { issues: { message: "部分字段未通过校验", fieldErrors } };
  }
  return Object.keys(increment).length > 0 ? { increment } : {};
}

const SKIP = { __skipToast: true } as const;

// —— descriptor 工厂（裸 CommandDescriptor，路径参数闭包——批 B editApiKeyDescriptor 先例）——

/** 创建：表单展示形在 run 内收敛（data/permissions）；重复键 409 → conflict。 */
export function createDocumentCommand(
  dbId: string,
  collId: string,
  attributes: Attribute[]
): CommandDescriptor<DocumentFormValues, Document> {
  return {
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
