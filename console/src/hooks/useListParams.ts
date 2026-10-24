import { useCallback, useMemo } from "react";
import { useSearchParams } from "@/router/compat";

export interface ListParams {
  q: string;
  page: number;
  pageSize: number;
  [key: string]: string | number;
}

const RESERVED = new Set(["q", "page", "pageSize"]);

export function useListParams(defaults?: Partial<ListParams>) {
  const [searchParams, setSearchParams] = useSearchParams();

  const params = useMemo((): ListParams => {
    const extra: Record<string, string> = {};
    searchParams.forEach((value, key) => {
      if (!RESERVED.has(key)) extra[key] = value;
    });
    return {
      q: searchParams.get("q") ?? defaults?.q ?? "",
      page: Number(searchParams.get("page") ?? defaults?.page ?? 1),
      pageSize: Number(searchParams.get("pageSize") ?? defaults?.pageSize ?? 20),
      ...extra,
    };
  }, [searchParams, defaults]);

  const setParams = useCallback(
    (updates: Partial<ListParams>, options?: { resetPage?: boolean }) => {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          const shouldResetPage =
            options?.resetPage ??
            Object.keys(updates).some((k) => k !== "page" && k !== "pageSize");

          Object.entries(updates).forEach(([key, value]) => {
            if (value === "" || value === undefined || value === null) {
              next.delete(key);
            } else {
              next.set(key, String(value));
            }
          });

          if (shouldResetPage && !("page" in updates)) {
            next.set("page", "1");
          }
          return next;
        },
        { replace: true }
      );
    },
    [setSearchParams]
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
