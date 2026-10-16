/**
 * 最小 ambient 声明：process.env 面。SDK 保持零 npm 依赖、不引入
 * @types/node；本包仅在 rpc.ts 的 makeContext 读 TW_API_BASE_URL（部署侧
 * 注入的平台 API base，Node ≥18 内置 process，package.json engines 已钉）。
 */
declare const process: {
  env: Record<string, string | undefined>;
};
