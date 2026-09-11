import { describe, expect, it } from "vitest";
import {
  EVENT_NAME_PATTERN,
  PROP_KEY_PATTERN,
  detailWindow,
  errText,
  formatBucket,
  granularityRange,
  num,
  overviewWindow,
  retentionFutureDay,
  retentionPercent,
  retentionWindow,
  timeseriesRows,
} from "./shared";

describe("num", () => {
  it("解析网关 int64 字符串与数字", () => {
    expect(num("123")).toBe(123);
    expect(num(42)).toBe(42);
    expect(num(undefined)).toBe(0);
    expect(num(null)).toBe(0);
    expect(num("")).toBe(0);
    expect(num("not-a-number")).toBe(0);
  });
});

describe("overviewWindow", () => {
  // 固定 now：2026-09-11T08:30:00Z
  const now = new Date("2026-09-11T08:30:00Z");

  it("today 从 UTC 零点起", () => {
    const w = overviewWindow("today", now);
    expect(w.start).toBe("2026-09-11T00:00:00.000Z");
    expect(w.end).toBe("2026-09-11T08:30:00.000Z");
  });

  it("7d/30d 为 now 往回等宽窗口", () => {
    expect(overviewWindow("7d", now).start).toBe("2026-09-04T08:30:00.000Z");
    expect(overviewWindow("30d", now).start).toBe("2026-08-12T08:30:00.000Z");
  });

  it("now 恰为 UTC 零点时 today 回退 1 小时保证 start < end", () => {
    const midnight = new Date("2026-09-11T00:00:00Z");
    const w = overviewWindow("today", midnight);
    expect(w.start).toBe("2026-09-10T23:00:00.000Z");
    expect(w.end).toBe("2026-09-11T00:00:00.000Z");
  });
});

describe("detailWindow / granularityRange", () => {
  const now = new Date("2026-09-11T08:30:00Z");

  it("24h/7d/30d 窗口", () => {
    expect(detailWindow("24h", now).start).toBe("2026-09-10T08:30:00.000Z");
    expect(detailWindow("7d", now).start).toBe("2026-09-04T08:30:00.000Z");
    expect(detailWindow("30d", now).start).toBe("2026-08-12T08:30:00.000Z");
  });

  it("HOUR 粒度下 30d 收敛为 7d（服务端护栏）", () => {
    expect(granularityRange("HOUR", "30d")).toBe("7d");
    expect(granularityRange("HOUR", "24h")).toBe("24h");
    expect(granularityRange("DAY", "30d")).toBe("30d");
  });
});

describe("formatBucket / timeseriesRows", () => {
  it("DAY 桶按 UTC 格式化为 MM-DD", () => {
    expect(formatBucket("2026-09-05T00:00:00Z", "DAY")).toBe("09-05");
  });

  it("HOUR 桶按 UTC 格式化为 MM-DD HH:00", () => {
    expect(formatBucket("2026-09-05T09:00:00Z", "HOUR")).toBe("09-05 09:00");
  });

  it("timeseriesRows 数值化 total/uv", () => {
    const rows = timeseriesRows(
      [
        { bucket: "2026-09-05T00:00:00Z", total: "12", unique_users: "7" },
        { bucket: "2026-09-06T00:00:00Z", total: 3 },
      ],
      "DAY"
    );
    expect(rows).toEqual([
      { label: "09-05", total: 12, uv: 7 },
      { label: "09-06", total: 3, uv: 0 },
    ]);
  });
});

describe("retention helpers", () => {
  it("retentionPercent 规模为 0 时返回 null", () => {
    expect(retentionPercent(5, 0)).toBeNull();
    expect(retentionPercent(50, 100)).toBe(50);
    expect(retentionPercent(1, 3)).toBe(33);
  });

  it("retentionFutureDay 标记 cohort+k 超过今日为未来日", () => {
    const now = new Date("2026-09-11T08:30:00Z");
    expect(retentionFutureDay("2026-09-10T00:00:00Z", 0, now)).toBe(false);
    expect(retentionFutureDay("2026-09-10T00:00:00Z", 1, now)).toBe(false);
    expect(retentionFutureDay("2026-09-10T00:00:00Z", 2, now)).toBe(true);
    expect(retentionFutureDay("2026-09-11T00:00:00Z", 0, now)).toBe(false);
    expect(retentionFutureDay("2026-09-11T00:00:00Z", 1, now)).toBe(true);
  });

  it("retentionWindow 覆盖 [now-Nd, 次日零点) 且不超过 92 天护栏基座", () => {
    const now = new Date("2026-09-11T08:30:00Z");
    const w = retentionWindow(30, now);
    expect(w.start).toBe("2026-08-13T00:00:00.000Z");
    expect(w.end).toBe("2026-09-12T00:00:00.000Z");
  });
});

describe("patterns", () => {
  it("事件名与 prop_key 白名单与 proto 同款", () => {
    expect(EVENT_NAME_PATTERN.test("level_up")).toBe(true);
    expect(EVENT_NAME_PATTERN.test("9start")).toBe(false);
    expect(EVENT_NAME_PATTERN.test("bad name")).toBe(false);
    expect(PROP_KEY_PATTERN.test("scene_id")).toBe(true);
    expect(PROP_KEY_PATTERN.test("2bad")).toBe(false);
    expect(PROP_KEY_PATTERN.test("a-b")).toBe(false);
  });
});

describe("errText", () => {
  it("优先取网关 ErrorResponse.error.message", () => {
    const e = { response: { data: { error: { message: "timeseries: window exceeds limit" } } } };
    expect(errText(e)).toBe("timeseries: window exceeds limit");
    expect(errText(new Error("boom"))).toBe("boom");
    expect(errText(undefined)).toBe("请求失败");
  });
});
