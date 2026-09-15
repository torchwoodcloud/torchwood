import { describe, expect, it } from "vitest";
import {
  browserTimezone,
  formatDateTime,
  formatDate,
  formatTimeOnly,
  fromDateTimeLocalValue,
  isValidTimezone,
  resolveTimezone,
  toDateTimeLocalValue,
} from "./datetime";

// 固定时刻：2026-09-15T07:30:45Z（NY 处于夏令时 EDT=-4，上海恒 +8）。
const T = "2026-09-15T07:30:45Z";

describe("resolveTimezone", () => {
  it("合法偏好原样生效", () => {
    expect(resolveTimezone("Asia/Shanghai")).toBe("Asia/Shanghai");
    expect(resolveTimezone("UTC")).toBe("UTC");
  });
  it("空/未设置/非法偏好回退浏览器时区", () => {
    expect(resolveTimezone(undefined)).toBe(browserTimezone());
    expect(resolveTimezone("")).toBe(browserTimezone());
    expect(resolveTimezone("Mars/Olympus")).toBe(browserTimezone());
    expect(resolveTimezone("Local")).toBe(browserTimezone());
  });
  it("isValidTimezone 走 Intl 支持集", () => {
    expect(isValidTimezone("Asia/Shanghai")).toBe(true);
    expect(isValidTimezone("Not/AZone")).toBe(false);
  });
});

describe("formatDateTime", () => {
  it("按指定时区显示同一绝对时刻", () => {
    expect(formatDateTime(T, "UTC")).toBe("2026-09-15 07:30:45");
    expect(formatDateTime(T, "Asia/Shanghai")).toBe("2026-09-15 15:30:45");
    expect(formatDateTime(T, "America/New_York")).toBe("2026-09-15 03:30:45");
  });
  it("偏好缺省回退浏览器时区（不抛错）", () => {
    expect(formatDateTime(T)).toBe(formatDateTime(T, browserTimezone()));
  });
  it("接受 Date 与毫秒数字输入", () => {
    expect(formatDateTime(new Date(T), "UTC")).toBe("2026-09-15 07:30:45");
    expect(formatDateTime(Date.parse(T), "UTC")).toBe("2026-09-15 07:30:45");
  });
  it("空值/无法解析返占位符", () => {
    expect(formatDateTime(undefined)).toBe("—");
    expect(formatDateTime(null)).toBe("—");
    expect(formatDateTime("")).toBe("—");
    expect(formatDateTime("not-a-date", "UTC")).toBe("—");
  });
});

describe("formatDate / formatTimeOnly", () => {
  it("日期与时刻分别格式化", () => {
    expect(formatDate(T, "Asia/Shanghai")).toBe("2026-09-15");
    expect(formatTimeOnly(T, "Asia/Shanghai")).toBe("15:30:45");
    // 近邻换日：UTC 已是次日而上海未到。
    expect(formatDate("2026-09-15T17:00:00Z", "UTC")).toBe("2026-09-15");
    expect(formatDate("2026-09-15T17:00:00Z", "Asia/Shanghai")).toBe(
      "2026-09-16"
    );
  });
});

describe("datetime-local 双向转换", () => {
  it("toDateTimeLocalValue 输出用户时区墙钟", () => {
    expect(toDateTimeLocalValue("2026-01-15T05:00:00Z", "America/New_York")).toBe(
      "2026-01-15T00:00"
    );
    expect(toDateTimeLocalValue(T, "Asia/Shanghai")).toBe("2026-09-15T15:30");
  });
  it("fromDateTimeLocalValue 按用户时区解释墙钟（标准时/夏令时）", () => {
    expect(fromDateTimeLocalValue("2026-01-15T00:00", "America/New_York")).toBe(
      "2026-01-15T05:00:00.000Z"
    );
    expect(fromDateTimeLocalValue("2026-07-15T00:00", "America/New_York")).toBe(
      "2026-07-15T04:00:00.000Z"
    );
    expect(fromDateTimeLocalValue("2026-09-15T15:30", "Asia/Shanghai")).toBe(
      "2026-09-15T07:30:00.000Z"
    );
  });
  it("墙钟 ↔ 时刻 往返一致（含南半球冬夏令时区）", () => {
    for (const tz of ["Asia/Shanghai", "America/New_York", "Australia/Sydney"]) {
      for (const instant of ["2026-01-15T05:00:00Z", "2026-07-15T05:00:00Z"]) {
        const local = toDateTimeLocalValue(instant, tz);
        expect(fromDateTimeLocalValue(local, tz)).toBe(
          new Date(instant).toISOString()
        );
      }
    }
  });
  it("非法输入返空串", () => {
    expect(toDateTimeLocalValue(undefined, "UTC")).toBe("");
    expect(fromDateTimeLocalValue("garbage", "UTC")).toBe("");
    expect(fromDateTimeLocalValue("", "UTC")).toBe("");
  });
});
