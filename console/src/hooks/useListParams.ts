import { useCallback, useMemo } from "react";
import { useNavigate, useSearch } from "@tanstack/react-router";

export interface ListParams {
  q: string;
  page: number;
  pageSize: number;
  [key: string]: string | number;
}

const RESERVED = new Set(["q", "page", "pageSize"]);

// 存量子视图列表（ResourceListPage 族）的本地分页/搜索参数钩子：URL 键值
// 以字符串面存取（TanStack search 对象 ↔ 平面 record 投影，语义与旧
// URLSearchParams 形等价——空值删除、resetPage 推页、replace 写入）。
export function useListParams(defaults?: Partial<ListParams>) {
  const search = useSearch({ strict: false }) as Record<string, unknown>;
  const navigate = useNavigate();

  const params = useMemo((): ListParams => {
    const extra: Record<string, string> = {};
    for (const [key, value] of Object.entries(search)) {
      if (RESERVED.has(key) || value === undefined || value === null) continue;
      extra[key] = String(value);
    }
    return {
      q: String(search.q ?? defaults?.q ?? ""),
      page: Number(search.page ?? defaults?.page ?? 1),
      pageSize: Number(search.pageSize ?? defaults?.pageSize ?? 20),
      ...extra,
    };
  }, [search, defaults]);

  const setParams = useCallback(
    (updates: Partial<ListParams>, options?: { resetPage?: boolean }) => {
      void navigate({
        to: ".",
        replace: true,
        search: (prev: Record<string, unknown>) => {
          const next: Record<string, string> = {};
          for (const [k, v] of Object.entries(prev)) {
            if (v !== undefined && v !== null) next[k] = String(v);
          }
          const shouldResetPage =
            options?.resetPage ??
            Object.keys(updates).some((k) => k !== "page" && k !== "pageSize");
          for (const [key, value] of Object.entries(updates)) {
            if (value === "" || value === undefined || value === null) {
              delete next[key];
            } else {
              next[key] = String(value);
            }
          }
          if (shouldResetPage && !("page" in updates)) {
            next.page = "1";
          }
          return next;
        },
      });
    },
    [navigate]
  );

  return { params, setParams };
}

export function paginate<T>(items: T[], page: number, pageSize: number) {
  const total = items.length;
  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const safePage = Math.min(Math.max(1, page), totalPages);
  const start = (safePage - 1) * pageSize;
  return {
    items: items.slice(start, start + pageSize),
    total,
    totalPages,
    page: safePage,
  };
}

export function filterByQuery<T>(
  items: T[],
  q: string,
  getSearchText: (item: T) => string
): T[] {
  const term = q.trim().toLowerCase();
  if (!term) return items;
  return items.filter((item) => getSearchText(item).toLowerCase().includes(term));
}
