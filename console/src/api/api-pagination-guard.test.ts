import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// 机制守卫（2026-09-21 assets 列表静默截断在服务端默认 25 条，CLI 可见全部）：
// api/ 下返回数组的列表封装必须对接服务端分页（经 pageQuery 传 page_size/
// page_token，返回 Page<T> 一页），否则必须在 ALLOWLIST 登记并写明理由。
// 防止下一个「裸 GET 第一页」的封装悄悄上线——dev 数据量小测不出来，数据过
// 服务端默认页大小后才在用户侧暴露。新增列表封装本测试变红时，按提示二选一处理。

const PAGINATION_MARKERS = /pageQuery\(|page_size|page_token|next_page_token/;

// 未接分页机制的登记处：每项注明理由；确认端点有分页后应迁出此表。
// （2026-09-22 P0-P2 迁移后仅剩静态资源/有意的全量/免分页子资源。）
const ALLOWLIST: Record<string, string[]> = {
  // functions 各列表（运行时/规格 = 进程内静态注册表；变量读回显 = PUT 写操作；
  // 触发器 = 单函数人手配置的量级）。
  "functions.ts": [
    "listRuntimes",
    "listSpecifications",
    "getVariables",
    // setVariables 是 PUT 写操作的读回显（数量 = 提交数），非列表查询。
    "setVariables",
    // 单函数的触发器配置子资源，量级由人手配决定。
    "listFunctionTriggers",
  ],
  // board/period 是元数据小列表（boards 每项目上限 100）；top 列表查询已走
  // page_size/page_token；settlements 已有显式 limit 参数，取多少由调用方决定。
  "leaderboards.ts": ["listBoards", "listBoardPeriods", "listSettlements"],
  // scope catalog 是静态资源枚举，非数据列表。
  "wellknown.ts": ["fetchApiKeyScopeCatalog"],
  // 变量/版本子列表为有意的全量拉取（设计决定 D13/D11）；var-sets 已迁。
  "runtimeVars.ts": ["listVars", "listVersions"],
  // 单用户会话子资源：服务端 ListUserSessions 尚无分页契约（P1 挂账）。
  "users.ts": ["listUserSessions"],
};

interface ListFn {
  file: string;
  name: string;
  body: string;
}

function extractListFunctions(): ListFn[] {
  // vitest jsdom 环境下 import.meta.url 非 file: scheme，用 cwd 定位
  // （vitest root = console/）。
  const dir = join(process.cwd(), "src", "api");
  const files = readdirSync(dir).filter(
    (f) => f.endsWith(".ts") && !f.endsWith(".test.ts")
  );
  const out: ListFn[] = [];
  for (const file of files) {
    const source = readFileSync(join(dir, file), "utf8");
    const re = /export async function (\w+)\([^)]*\): Promise<[^<>]*\[\]> \{/g;
    const matches = [...source.matchAll(re)];
    matches.forEach((m, i) => {
      const start = m.index ?? 0;
      const end = matches[i + 1]?.index ?? source.length;
      out.push({ file, name: String(m[1]), body: source.slice(start, end) });
    });
  }
  return out;
}

describe("api 列表封装分页守卫", () => {
  it("每个列表封装要么带分页标记，要么在 ALLOWLIST 登记", () => {
    const fns = extractListFunctions();
    expect(fns.length).toBeGreaterThan(0);

    const unguarded = fns.filter(
      (fn) =>
        !PAGINATION_MARKERS.test(fn.body) &&
        !(ALLOWLIST[fn.file] ?? []).includes(fn.name)
    );
    expect(
      unguarded.map((fn) => `${fn.file}:${fn.name}`),
      "以下列表封装裸 GET 第一页，会静默截断在服务端默认 page_size。请对接服务端" +
        "分页（经 pageQuery 传 page_size/page_token，返回 Page<T>，UI 经 " +
        "useServerPaging 翻页）；确属不分页/小列表则在 ALLOWLIST 登记并注明理由。"
    ).toEqual([]);
  });

  it("ALLOWLIST 不得含死条目（函数已删/改名后必须清理）", () => {
    const actual = new Set(
      extractListFunctions().map((fn) => `${fn.file}:${fn.name}`)
    );
    const dead: string[] = [];
    for (const [file, names] of Object.entries(ALLOWLIST)) {
      for (const name of names) {
        if (!actual.has(`${file}:${name}`)) dead.push(`${file}:${name}`);
      }
    }
    expect(dead).toEqual([]);
  });
});
