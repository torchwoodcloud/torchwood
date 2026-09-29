import { useCallback, useEffect, useRef, useState } from "react";

// 服务端 keyset 分页的通用游标状态：token 栈（栈底 = 第一页的空 token），
// 前进压栈、后退弹栈；page 序号仅供展示（keyset 下不能跳页）。
// resetKeys：任一元素变化即回第一页（过滤/换向等服务端参数变化）——
// 「参数变即回第一页」从各页面手动 reset() 的纪律收敛为机制（挂载首跑与
// 值未变的重渲染为 no-op）；pageSize 变更内置即 reset。
export function useServerPaging(
  defaultPageSize = 20,
  opts?: { resetKeys?: readonly unknown[] }
) {
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

  // resetKeys 逐元素 Object.is 比较（ref 上一份 + 无依赖 effect：调用方传
  // 内联数组也不会每渲染误触发；省显式依赖数组以绕开 exhaustive-deps 对
  // 非字面量数组的约束）。
  const resetKeys = opts?.resetKeys;
  const prevKeysRef = useRef<readonly unknown[] | undefined>(undefined);
  useEffect(() => {
    const prev = prevKeysRef.current;
    prevKeysRef.current = resetKeys;
    if (!resetKeys) return;
    if (
      prev &&
      prev.length === resetKeys.length &&
      prev.every((v, i) => Object.is(v, resetKeys[i]))
    ) {
      return;
    }
    setTokenStack([""]);
  });

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
