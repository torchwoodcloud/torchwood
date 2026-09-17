// 共享时间格式化：所有时间字段按"管理员时区偏好"显示（账户设置页里改），
// 未设置时回退浏览器时区。API 返回 RFC3339 绝对时刻，这里只做显示层转换。
// Analytics 桶标签是刻意的 UTC 口径，不走这里（见 routes/analytics/shared.ts）。
// 不引入日期库：Intl 原生支持 IANA 时区。

const BROWSER_TIMEZONE = (() => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
})();

// 全量 IANA 时区集合（偏好选择器数据源）。注意 supportedValuesOf 不含 "UTC"
// （ECMA-402 单独保证其永远合法），需显式补入。旧浏览器无 supportedValuesOf
// 时退化为仅浏览器时区 + UTC。
const SUPPORTED_TIMEZONES: readonly string[] = (() => {
  if (typeof Intl.supportedValuesOf !== "function") {
    return ["UTC", BROWSER_TIMEZONE];
  }
  try {
    return ["UTC", ...new Set(Intl.supportedValuesOf("timeZone"))];
  } catch {
    return ["UTC", BROWSER_TIMEZONE];
  }
})();

const validTimezoneCache = new Map<string, boolean>();

// intlAccepts 用 Intl 实测（RangeError = 不认识），比白名单更稳：
// supportedValuesOf 与 DateTimeFormat 的接受面在实现间可能不一致。
function intlAccepts(tz: string): boolean {
  let ok = validTimezoneCache.get(tz);
  if (ok === undefined) {
    try {
      new Intl.DateTimeFormat("en-CA", { timeZone: tz });
      ok = true;
    } catch {
      ok = false;
    }
    validTimezoneCache.set(tz, ok);
  }
  return ok;
}

export function browserTimezone(): string {
  return BROWSER_TIMEZONE;
}

// 支持设置的 IANA 时区列表（偏好选择器数据源，字典序稳定）。
export function supportedTimezones(): string[] {
  return [...SUPPORTED_TIMEZONES].sort();
}

export function isValidTimezone(tz: string): boolean {
  return intlAccepts(tz);
}

// resolveTimezone 偏好 → 浏览器 → UTC 回退链。偏好非法（旧数据/环境变化后
// 时区库收紧）时静默回退，绝不让 Intl 抛 RangeError 打断渲染。
export function resolveTimezone(pref?: string | null): string {
  if (pref && intlAccepts(pref)) return pref;
  return BROWSER_TIMEZONE;
}

// formatter 缓存（Intl.DateTimeFormat 构造较重；列表/表格每次渲染都会调用）。
const formatterCache = new Map<string, Intl.DateTimeFormat>();

// zonedParts 固定数字格式（en-CA + h23 产出 yyyy-MM-dd HH:mm:ss 形状），
// 与浏览器 locale / 12/24 小时制设置无关。
function zonedParts(
  date: Date,
  tz: string,
  withTime: boolean
): Record<string, string> {
  const key = `${tz}|${withTime}`;
  let fmt = formatterCache.get(key);
  if (!fmt) {
    fmt = new Intl.DateTimeFormat("en-CA", {
      timeZone: tz,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      ...(withTime
        ? { hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" as const }
        : {}),
    });
    formatterCache.set(key, fmt);
  }
  const map: Record<string, string> = {};
  for (const p of fmt.formatToParts(date)) map[p.type] = p.value;
  return map;
}

function toDate(value: string | number | Date): Date | null {
  const d = value instanceof Date ? value : new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
}

function formatDateStr(map: Record<string, string>): string {
  return `${map.year}-${map.month}-${map.day}`;
}

function formatTimeStr(map: Record<string, string>): string {
  return `${map.hour}:${map.minute}:${map.second}`;
}

// formatDateTime "yyyy-MM-dd HH:mm:ss"；空值/无法解析返 "—"。
export function formatDateTime(
  value?: string | number | Date | null,
  pref?: string | null
): string {
  const d = value == null ? null : toDate(value);
  if (!d) return "—";
  const map = zonedParts(d, resolveTimezone(pref), true);
  return `${formatDateStr(map)} ${formatTimeStr(map)}`;
}

// formatDate "yyyy-MM-dd"。
export function formatDate(
  value?: string | number | Date | null,
  pref?: string | null
): string {
  const d = value == null ? null : toDate(value);
  if (!d) return "—";
  return formatDateStr(zonedParts(d, resolveTimezone(pref), false));
}

// formatTimeOnly "HH:mm:ss"。
export function formatTimeOnly(
  value?: string | number | Date | null,
  pref?: string | null
): string {
  const d = value == null ? null : toDate(value);
  if (!d) return "—";
  return formatTimeStr(zonedParts(d, resolveTimezone(pref), true));
}

// getTimezoneOffsetMs 该时刻在 tz 的墙钟（按 UTC 解释）与真实 UTC 时刻之差。
function getTimezoneOffsetMs(date: Date, tz: string): number {
  const map = zonedParts(date, tz, true);
  const asUTC = Date.UTC(
    Number(map.year),
    Number(map.month) - 1,
    Number(map.day),
    Number(map.hour),
    Number(map.minute),
    Number(map.second)
  );
  return asUTC - date.getTime();
}

// toDateTimeLocalValue 绝对时刻 → 用户时区墙钟的 datetime-local 值
// （"yyyy-MM-ddTHH:mm"），供 <input type="datetime-local"> 显示。
export function toDateTimeLocalValue(
  value?: string | number | Date | null,
  pref?: string | null
): string {
  const d = value == null ? null : toDate(value);
  if (!d) return "";
  const map = zonedParts(d, resolveTimezone(pref), true);
  return `${formatDateStr(map)}T${map.hour}:${map.minute}`;
}

// fromDateTimeLocalValue datetime-local 墙钟值按用户时区解释 → 绝对时刻
// （toISOString 形状）；空/无法解析返 ""。两段式求偏移收敛 DST 边界。
export function fromDateTimeLocalValue(
  local: string,
  pref?: string | null
): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(local);
  if (!m) return "";
  const tz = resolveTimezone(pref);
  const [, y, mo, d, h, mi, s] = m;
  const guess = Date.UTC(
    Number(y), Number(mo) - 1, Number(d), Number(h), Number(mi), Number(s ?? 0)
  );
  let offset = getTimezoneOffsetMs(new Date(guess), tz);
  offset = getTimezoneOffsetMs(new Date(guess - offset), tz);
  return new Date(guess - offset).toISOString();
}
