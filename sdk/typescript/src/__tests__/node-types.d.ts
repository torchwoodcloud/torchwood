// 最小 ambient 声明：Node 内置测试运行器（node:test / node:assert）的
// 运行时模块声明，避免为 SDK 引入 @types/node 依赖。
declare module "node:test" {
  export function describe(name: string, fn: () => void): void;
  export function it(name: string, fn: () => void | Promise<void>): void;
  export function it(
    name: string,
    options: unknown,
    fn: () => void | Promise<void>
  ): void;
  export function beforeEach(fn: () => void | Promise<void>): void;
  export function afterEach(fn: () => void | Promise<void>): void;
}

declare module "node:assert/strict" {
  interface Assert {
    ok(value: unknown, message?: string): void;
    equal(actual: unknown, expected: unknown, message?: string): void;
    deepEqual(actual: unknown, expected: unknown, message?: string): void;
    throws(fn: () => void, error?: unknown): void;
    rejects(fn: () => Promise<unknown>, error?: unknown): Promise<void>;
  }
  const assert: Assert;
  export default assert;
}

declare module "node:fs" {
  export function readFileSync(path: string, encoding: string): string;
  export function readdirSync(path: string): string[];
  export function existsSync(path: string): boolean;
}

declare module "node:path" {
  export function join(...parts: string[]): string;
  export function dirname(path: string): string;
}

declare module "node:url" {
  export function fileURLToPath(url: string): string;
}

// Node 运行时全局（测试需要读 TW_EXECUTION_TOKEN env）。
declare var process: {
  env: Record<string, string | undefined>;
};
