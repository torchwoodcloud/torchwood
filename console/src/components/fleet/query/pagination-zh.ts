// 分页块中文文案单源（§12.10-⑤ labels 注入面）：上游块默认英文，
// 各页经 <KeysetPagination labels={PAGINATION_ZH} …> 注入；页大小选项
// 沿用与服务端 clamp 上限一致的既有词表。
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import type { KeysetPaginationLabels } from "./keyset-pagination";

export const PAGINATION_ZH: Partial<KeysetPaginationLabels> = {
  describe: ({ rows, total, hasMore }) =>
    total !== undefined
      ? `共 ${total} 条 · 本页 ${rows} 条`
      : rows === 0
        ? "本页无记录"
        : `本页 ${rows} 条${hasMore ? "" : "（已到底）"}`,
  prev: "上一页",
  next: "下一页",
  pageSizeAria: "页大小",
  pageSizeOption: (size) => `${size} 条/页`,
};

export { PAGE_SIZE_OPTIONS };
