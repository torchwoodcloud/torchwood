// pages.tsx 已按页面拆分（P3-12）：按职责拆为 databases.tsx / collections.tsx / documents.tsx / components.tsx
// 本文件保留为 barrel，便于既有 import { DatabasesListPage } from "@/routes/databases/pages" 兼容。
// 显式导出（react-refresh 规则无法验证 export * 的导出面）。
export {
  DatabasesListPage,
  DatabaseNewPage,
  DatabaseDetailPage,
} from "./databases";
export {
  CollectionNewPage,
  CollectionDetailPage,
} from "./collections";
export {
  DocumentsListPage,
  DocumentNewPage,
  DocumentDetailPage,
} from "./documents";
export {
  AttributeList,
  IndexList,
  AddAttributeDialog,
  AddIndexDialog,
  EditPermissionsDialog,
  EditCollectionDialog,
} from "./components";
