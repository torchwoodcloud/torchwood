import {
  type CollectionQuery,
  type CommandDescriptor,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  createVar,
  createVarSet,
  deleteVar,
  deleteVarSet,
  getVarSet,
  getVersion,
  listVarSets,
  listVars,
  listVersions,
  rollback,
  updateVar,
  updateVarSet,
  type GetRuntimeVarVersionResponse,
  type RuntimeVar,
  type RuntimeVarValueInput,
  type RuntimeVarVersion,
  type VarSet,
  type VarSetVisibilityInput,
} from "@/api/runtimeVars";
import { recordOrNull, runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：var-sets 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（kit/errors.ts 收敛缝）。
//
// 命令族切片（写路径命令化）：全部写动词挂 varSetsResource——var 写会 bump
// 集合 revision/var_count/footprint，列表（kit 键）随资源命令 ok 自动失效。
// 子视图 kit 化批：详情读（getVarSet）挂 varSetsResource.record；vars/versions
// 子视图仍为有意的全量拉取（设计决定 D13/D11）——runtime-vars /
// runtime-var-versions collection 整取方言 + runtime-var-version 快照 record
// 槽（record-only 资源，见其声明点说明）。
// 存量前缀桥（§12.10-②）已退役（读者清零）：var 级动词与裸工厂的子视图
// 键族改由 descriptor.invalidates 声明（kit 键全 kind 命中）。
// 并发面 = LWW（design.md §12.5 已证：UpdateVarSet/UpdateVar 等无写前件），
// 不设 OCC 守卫；结果两分映射同 apiKeys（400→rejected、409→conflict、其余
// throw ApiError）；api 调用一律 __skipToast（toast 单源 = onSettled）。

// —— 新建集合：表单值（展示形）在 run 内原样收敛为提交形 ——

export interface CreateVarSetForm {
  varSetId: string;
  visibility: VarSetVisibilityInput;
  description: string;
}

// —— 集合设置编辑：diff 语义归 descriptor（visibility 未变化不发送 =
// proto3 optional 不修改；description 为全量表单值）——

export interface EditVarSetForm {
  visibility: VarSetVisibilityInput;
  description: string;
}

function toVisibility(v: string): VarSetVisibilityInput {
  return v === "VAR_SET_VISIBILITY_PRIVATE"
    ? "VAR_SET_VISIBILITY_PRIVATE"
    : "VAR_SET_VISIBILITY_PUBLIC";
}

/** descriptor 工厂：闭包原实体做 visibility diff（LWW——无 OCC 前置守卫）。 */
export function editVarSetDescriptor(varSet: VarSet): CommandDescriptor<EditVarSetForm, VarSet> {
  return {
    run: async (form) =>
      runCommandMapped(() =>
        updateVarSet(
          varSet.var_set_id,
          {
            // proto3 optional：未变化的 visibility 不发送 = 不修改。
            visibility:
              form.visibility !== toVisibility(varSet.visibility) ? form.visibility : undefined,
            description: form.description,
          },
          SKIP
        )
      ),
    // 裸工厂无 __resource：kit 键显式声明失效。真实变更面 = 集合元数据
    //（visibility/description，D10 不入版本链）→ 收窄为仅 var-sets（列表 +
    // 详情 record 全 kind）；旧 legacyKeys 宽桥连带刷 versions 属过宽，退役。
    invalidates: [{ resource: varSetsResource.name }],
  };
}

// —— 变量新建/编辑：五类型值控件的展示形在 run 内收敛为 oneof 提交形 ——
//（json 的 JSON.parse 校验、integer ±2^53−1 与 protovalidate 对齐——违规落
// rejected 结果分支，不占失败通道）。

export type VarKind = "string" | "integer" | "float" | "boolean" | "json";

/** 变量表单值展示形（值控件四槽；bool 为 select 字符串 "true"/"false"）。 */
export interface VarFormValues {
  /** 创建用（编辑时 key 锁定，走 editVarDescriptor 闭包）。 */
  key: string;
  kind: VarKind;
  str: string;
  num: string;
  bool: string;
  json: string;
  description: string;
}

export interface VarSubmitInput extends VarFormValues {
  varSetId: string;
}

const SAFE_INTEGER_LIMIT = 9007199254740991;

function buildVarValue(form: VarFormValues): { value?: RuntimeVarValueInput; error?: string } {
  switch (form.kind) {
    case "string":
      return { value: { string_value: form.str } };
    case "integer": {
      if (!form.num.trim()) return { error: "Integer cannot be empty" };
      const n = Number(form.num);
      if (!Number.isInteger(n)) return { error: "Not a valid integer" };
      if (Math.abs(n) > SAFE_INTEGER_LIMIT) {
        return { error: "Out of safe integer range (±2^53−1)" };
      }
      return { value: { integer_value: n } };
    }
    case "float": {
      if (!form.num.trim()) return { error: "Number cannot be empty" };
      const n = Number(form.num);
      if (!Number.isFinite(n)) return { error: "Not a valid number" };
      return { value: { float_value: n } };
    }
    case "boolean":
      return { value: { bool_value: form.bool === "true" } };
    case "json": {
      if (!form.json.trim()) return { error: "JSON cannot be empty" };
      try {
        JSON.parse(form.json);
      } catch (e) {
        return {
          error: `Invalid JSON format: ${e instanceof Error ? e.message : String(e)}`,
        };
      }
      return { value: { json_value: form.json } };
    }
  }
}

/** 编辑 descriptor 工厂：key 创建后不可改——闭包原实体 key（值/描述全量提交）。 */
export function editVarDescriptor(
  varSetId: string,
  key: string
): CommandDescriptor<VarSubmitInput, RuntimeVar> {
  return {
    run: async (input) => {
      const built = buildVarValue(input);
      const value = built.value;
      if (!value) return { status: "rejected", message: built.error ?? "Invalid value" };
      return runCommandMapped(() =>
        updateVar(varSetId, key, { value, description: input.description }, SKIP)
      );
    },
    // var 写 bump 集合 revision/var_count/footprint（新版本入链）——三个资源
    // 的 kit 键（含详情 record/版本 record 全 kind）一并显式失效（字符串字面量
    // ——资源声明在本文件后段，模块初始化期求值避免 TDZ）。
    invalidates: [
      { resource: "runtime-var-sets" },
      { resource: "runtime-vars" },
      { resource: "runtime-var-versions" },
    ],
  };
}

// —— 列表 ——

// —— runtime-vars（集合详情内嵌变量列表；嵌套 scope 加 varSetId，ctx 缺参静默空段）——

// D13：变量整取端点（≤500 keys/集合，page_size 1000 一页取全）无谓词/游标
// 参数——查询窗口忽略，行全量直出（整取方言，页面 window: "all"）。
async function fetchVars(
  _q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<RuntimeVar>> {
  if (ctx.projectId == null || ctx.varSetId == null) return { rows: [] };
  try {
    return { rows: await listVars(ctx.varSetId) };
  } catch (err) {
    throw toApiError(err);
  }
}

export const runtimeVarsResource = defineResource({
  name: "runtime-vars",
  // RuntimeVar 无 id 字段——key 创建后不可改（D3），即自然主键。
  getId: (row: RuntimeVar) => row.key,
  scope: ["projectId", "varSetId"],
  collection: fetchVars,
});

// —— runtime-var-versions（版本历史列表 + 版本快照详情；嵌套 scope 同上）——

// D11：版本链整取端点（保留最近 50 版，page_size 200 一页取全）无谓词/游标
// 参数——同上整取方言。
async function fetchVersions(
  _q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<RuntimeVarVersion>> {
  if (ctx.projectId == null || ctx.varSetId == null) return { rows: [] };
  try {
    return { rows: await listVersions(ctx.varSetId) };
  } catch (err) {
    throw toApiError(err);
  }
}

export const runtimeVarVersionsResource = defineResource({
  name: "runtime-var-versions",
  // 版本主键 = revision（int64 已在 api 层归一 number）。
  getId: (row: RuntimeVarVersion) => row.revision,
  scope: ["projectId", "varSetId"],
  collection: fetchVersions,
});

// —— runtime-var-version（版本快照全文单读；record-only 资源）——
// 资源拆分说明：kit 资源单 T 契约钉死 record 返回型 = collection 行型，而
// 快照 GetRuntimeVarVersionResponse ≠ 版本元数据 RuntimeVarVersion——拆出
// record-only 资源，命名恰 = 旧 legacy 快照前缀首段（单数形）。快照内容不可
// 变（api 层契约）：命令 invalidates 不含本资源（旧宽桥连带刷属过宽，收窄；
// 新 revision 的快照键无缓存自然 fresh fetch）。
export const runtimeVarVersionResource = defineResource({
  name: "runtime-var-version",
  scope: ["projectId", "varSetId"],
  // 版本快照详情读（VersionSnapshotDialog / VersionDiffDialog，子视图 kit 化
  // 批）：404 → null 空态直出（recordOrNull 手写方言，不引 restRecord）；
  // id = revision；ctx.varSetId 由 scopeReady 就绪门保证（查询未就绪即停用），
  // ! 仅类型收窄。staleTime 维持缺省（行为保真）。
  record: (id: number, ctx: ResourceScope) =>
    recordOrNull(() => getVersion(ctx.varSetId!, id)) as Promise<GetRuntimeVarVersionResponse>,
});

async function fetchVarSets(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<VarSet>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listVarSets({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const varSetsResource = defineResource({
  name: "runtime-var-sets",
  getId: (row: VarSet) => row.id,
  scope: ["projectId"],
  collection: fetchVarSets,
  // 详情读（VarSetDetailPage，子视图 kit 化批）：404 → null 空态直出
  //（recordOrNull 手写方言，不引 restRecord）。
  record: (id) => recordOrNull(() => getVarSet(id)) as Promise<VarSet>,
  // 存量键失效桥已退役（子视图 kit 化批）：["runtime-var-sets"] 前缀读者
  // 清零（列表走 kit 集合键、详情读换轨 kit record 键，同资源缺省失效全
  // kind 命中）。
  commands: {
    // 新建集合（var_set_id 创建后不可改，D9）
    create: {
      run: async (form: CreateVarSetForm): Promise<CommandResult<VarSet>> =>
        runCommandMapped(() =>
          createVarSet(
            {
              var_set_id: form.varSetId,
              visibility: form.visibility,
              description: form.description,
            },
            SKIP
          )
        ),
    },
    // 删除已删 → 幂等 ok（apiKeys/documents remove 先例）
    remove: {
      run: async (input: { varSetId: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteVarSet(input.varSetId, SKIP);
          return null;
        }),
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
    // 新建变量：值展示形在 run 内收敛（五类型 oneof + json 校验）
    varCreate: {
      run: async (input: VarSubmitInput): Promise<CommandResult<RuntimeVar>> => {
        const built = buildVarValue(input);
        const value = built.value;
        if (!value) return { status: "rejected", message: built.error ?? "Invalid value" };
        return runCommandMapped(() =>
          createVar(
            input.varSetId,
            { key: input.key, value, description: input.description },
            SKIP
          )
        );
      },
      // var 写 bump revision/var_count/footprint——三资源 kit 键全失效
      //（invalidates 声明优先 = 整体替换缺省，同资源显式带上）。
      invalidates: [
        { resource: "runtime-var-sets" },
        { resource: "runtime-vars" },
        { resource: "runtime-var-versions" },
      ],
    },
    // 删除变量：已删 → 幂等 ok
    varRemove: {
      run: async (input: {
        varSetId: string;
        key: string;
      }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteVar(input.varSetId, input.key, SKIP);
          return null;
        }),
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
      invalidates: [
        { resource: "runtime-var-sets" },
        { resource: "runtime-vars" },
        { resource: "runtime-var-versions" },
      ],
    },
    // 回滚到目标版本（custom verb）；目标=当前版本 → 400 InvalidArgument（D12）
    // → rejected；窗口淘汰 → 404（失败通道，describeApiError）。
    // 真实变更面 = 全量变量替换 + 新版本入链 + 集合 revision bump → 三资源全失效。
    rollback: {
      run: async (input: {
        varSetId: string;
        targetRevision: number;
      }): Promise<CommandResult<RuntimeVarVersion>> =>
        runCommandMapped(() => rollback(input.varSetId, input.targetRevision, SKIP)),
      invalidates: [
        { resource: "runtime-var-sets" },
        { resource: "runtime-vars" },
        { resource: "runtime-var-versions" },
      ],
    },
  },
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；集合列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const varSetsSearch = collectionQuerySchema({ defaultSize: 20 });

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createVarSetCommand = varSetsResource.commands.create;
export const removeVarSetCommand = varSetsResource.commands.remove;
export const createVarCommand = varSetsResource.commands.varCreate;
export const removeVarCommand = varSetsResource.commands.varRemove;
export const rollbackVarSetCommand = varSetsResource.commands.rollback;
