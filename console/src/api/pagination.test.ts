import { describe, expect, it } from "vitest";

import { pageQuery, sortOrderParam } from "./pagination";

// 回归：grpc-gateway 按 proto 枚举名解析 query 枚举，直传 "DESC"/"ASC"
// 会 400（parsing field "sort_order": … is not a valid value）。
describe("sortOrderParam", () => {
  it("翻译为 proto 枚举名", () => {
    expect(sortOrderParam("ASC")).toBe("SORT_ORDER_ASC");
    expect(sortOrderParam("DESC")).toBe("SORT_ORDER_DESC");
  });
});

describe("pageQuery", () => {
  it("sort_order 翻译为 proto 枚举名", () => {
    expect(pageQuery({ pageSize: 20, sortOrder: "DESC" }).sort_order).toBe(
      "SORT_ORDER_DESC"
    );
    expect(pageQuery({ pageSize: 20, sortOrder: "ASC" }).sort_order).toBe(
      "SORT_ORDER_ASC"
    );
  });

  it("缺省 sortOrder 时 sort_order 不下发", () => {
    expect(pageQuery({ pageSize: 20 }).sort_order).toBeUndefined();
  });
});
