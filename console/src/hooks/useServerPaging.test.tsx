import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { useServerPaging } from "./useServerPaging";

describe("useServerPaging", () => {
  it("token 栈：前进压栈、后退弹栈、到底停住", () => {
    const { result } = renderHook(() => useServerPaging());
    expect(result.current.pageToken).toBe("");
    expect(result.current.page).toBe(1);
    expect(result.current.hasPrev).toBe(false);

    act(() => result.current.goNext("t2"));
    expect(result.current.pageToken).toBe("t2");
    expect(result.current.page).toBe(2);
    expect(result.current.hasPrev).toBe(true);

    act(() => result.current.goNext("t3"));
    expect(result.current.pageToken).toBe("t3");
    act(() => result.current.goPrev());
    expect(result.current.pageToken).toBe("t2");
    act(() => result.current.goPrev());
    act(() => result.current.goPrev());
    expect(result.current.pageToken).toBe("");
    expect(result.current.hasPrev).toBe(false);
  });

  it("goNext 空 token 不压栈（终止契约：末页无 next）", () => {
    const { result } = renderHook(() => useServerPaging());
    act(() => result.current.goNext(undefined));
    act(() => result.current.goNext(""));
    expect(result.current.page).toBe(1);
  });

  it("setPageSize 换页大小即回第一页", () => {
    const { result } = renderHook(() => useServerPaging(20));
    act(() => result.current.goNext("t2"));
    act(() => result.current.setPageSize(50));
    expect(result.current.pageSize).toBe(50);
    expect(result.current.pageToken).toBe("");
    expect(result.current.page).toBe(1);
  });

  it("resetKeys：任一值变化即回第一页", () => {
    const { result, rerender } = renderHook(
      ({ filter, order }: { filter: string; order: string }) =>
        useServerPaging(20, { resetKeys: [filter, order] }),
      { initialProps: { filter: "", order: "DESC" } }
    );
    act(() => result.current.goNext("t2"));
    expect(result.current.pageToken).toBe("t2");

    // 过滤变化 → reset。
    rerender({ filter: "status=denied", order: "DESC" });
    expect(result.current.pageToken).toBe("");

    // 再前进后，仅重渲染（值未变）不 reset。
    act(() => result.current.goNext("t3"));
    rerender({ filter: "status=denied", order: "DESC" });
    expect(result.current.pageToken).toBe("t3");

    // 换向 → reset。
    rerender({ filter: "status=denied", order: "ASC" });
    expect(result.current.pageToken).toBe("");
  });

  it("无 resetKeys 时不自动 reset", () => {
    const { result, rerender } = renderHook(() => useServerPaging());
    act(() => result.current.goNext("t2"));
    rerender();
    expect(result.current.pageToken).toBe("t2");
  });

  it("reset() 手动回第一页", () => {
    const { result } = renderHook(() => useServerPaging());
    act(() => result.current.goNext("t2"));
    act(() => result.current.reset());
    expect(result.current.pageToken).toBe("");
  });
});
