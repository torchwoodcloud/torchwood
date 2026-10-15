/**
 * 最小 ambient 声明：`node:crypto` 的 HMAC 面。SDK 保持零 npm 依赖、不引入
 * @types/node（对齐 src/__tests__/node-types.d.ts 惯例）；本包仅在 verify.ts
 * 用 createHmac 计算协议签名（Node ≥18 内置，package.json engines 已钉）。
 */
declare module "node:crypto" {
  export interface Hmac {
    update(data: string | Uint8Array): this;
    digest(encoding: "hex"): string;
  }
  export function createHmac(algorithm: string, key: string | Uint8Array): Hmac;
}
