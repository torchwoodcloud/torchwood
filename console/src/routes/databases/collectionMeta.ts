// 集合/文档的元数据常量与表单工具（非组件成员从 components.tsx 拆出，
// 满足 react-refresh 只导出组件的约束）。
import type { Attribute, Document } from "@/api/databases";

export const ATTRIBUTE_TYPES = [
  { value: "string", label: "String" },
  { value: "integer", label: "Integer" },
  { value: "float", label: "Float" },
  { value: "boolean", label: "Boolean" },
  { value: "datetime", label: "Datetime" },
  { value: "email", label: "Email" },
  { value: "url", label: "URL" },
  { value: "json", label: "JSON" },
] as const;

export const INDEX_TYPES = [
  { value: "key", label: "Key" },
  { value: "unique", label: "Unique" },
  { value: "fulltext", label: "Fulltext" },
] as const;

export const STRING_LIKE_TYPES = new Set(["string", "email", "url"]);

// 与 internal/app/server/databases.go 的 maxBulkOperations 保持一致
export const MAX_BULK_OPERATIONS = 1000;

// documentToValues 将服务端文档反序列化为表单字符串值（与初始化守卫共用，
// 保存成功后用响应文档重建表单，避免与服务端状态失同步）。
export function documentToValues(
  attributes: Attribute[],
  doc: Document
): Record<string, string> {
  const next: Record<string, string> = {};
  if (attributes.length === 0) {
    next.__json = JSON.stringify(doc.data ?? {}, null, 2);
  } else {
    for (const attr of attributes) {
      const raw = doc.data?.[attr.key];
      if (raw == null) {
        next[attr.key] = "";
      } else if (attr.type === "json" || typeof raw === "object") {
        // json 属性的值可能是任意 JSON 标量/对象，统一以 JSON 字面量呈现，
        // 与 parseFieldValue 的 JSON.parse 往返一致；对象走 String() 会变成 "[object Object]"。
        next[attr.key] = JSON.stringify(raw, null, 2);
      } else {
        next[attr.key] = String(raw);
      }
    }
  }
  return next;
}
