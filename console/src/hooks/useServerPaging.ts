import { useCallback, useState } from "react";

// 服务端 keyset 分页的通用游标状态：token 栈（栈底 = 第一页的空 token），
// 前进压栈、后退弹栈；page 序号仅供展示（keyset 下不能跳页）。
// 先例：audit-logs/pages.tsx。搜索/筛选等服务端参数变化时调用 reset() 回第一页。
export function useServerPaging(defaultPageSize = 20) {
  const [tokenStack, setTokenStack] = useState<string[]>([""]);
  const [pageSize, setPageSizeState] = useState(defaultPageSize);

  const pageToken = tokenStack[tokenStack.length - 1] ?? "";
  const page = tokenStack.length;

  const goNext = useCallback((nextToken?: string) => {
    if (nextToken) setTokenStack((s) => [...s, nextToken]);
  }, []);
  const goPrev = useCallback(() => {
    setTokenStack((s) => (s.length > 1 ? s.slice(0, -1) : s));
  }, []);
  const reset = useCallback(() => setTokenStack([""]), []);
  const setPageSize = useCallback(
    (n: number) => {
      setPageSizeState(n);
      setTokenStack([""]);
    },
    []
  );

  return {
    pageToken,
    page,
    pageSize,
    setPageSize,
    goNext,
    goPrev,
    hasPrev: tokenStack.length > 1,
    reset,
  };
}
