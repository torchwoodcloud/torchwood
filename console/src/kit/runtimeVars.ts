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
  listVarSets,
  rollback,
  updateVar,
  updateVarSet,
  type RuntimeVar,
  type RuntimeVarValueInput,
  type RuntimeVarVersion,
  type VarSet,
  type VarSetVisibilityInput,
} from "@/api/runtimeVars";
import { runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：var-sets 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。vars/versions 子视图仍为有意
// 的全量拉取（设计决定 D13/D11），不在此列。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（kit/errors.ts 收敛缝）。
//
// 命令族切片（写路径命令化）：全部写动词挂 varSetsResource——var 写会 bump
// 集合 revision/var_count/footprint，列表（kit 键）随资源命令 ok 自动失效；
// 存量键失效桥（§12.10-②）：集合级动词（create/remove）经资源 legacyKeys
// 失效 ["runtime-var-sets"] 前缀（宽前缀）；var 级动词（varCreate/varRemove/
// rollback）与裸工厂（editVar/editVarSet）在 descriptor 级声明全族前缀键
//（列表/详情/变量/版本/版本快照）——页面 useInvalidateVarSet 手动桥退役。
// 并发面 = LWW（design.md §12.5 已证：UpdateVarSet/UpdateVar 等无写前件），
// 不设 OCC 守卫；结果两分映射同 apiKeys（400→rejected、409→conflict、其余
// throw ApiError）；api 调用一律 __skipToast（toast 单源 = onSettled）。

/** var 级写的存量子视图键族（宽前缀，projectId/varSetId 形全覆盖）。 */
const varSetSubviewKeys = () => [
  ["runtime-var-sets"],
  ["runtime-var-set"],
  ["runtime-vars"],
  ["runtime-var-versions"],
  ["runtime-var-version"],
];

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
    // 裸工厂无 __resource：kit 列表键（visibility 列会变）显式声明失效；
    // 存量子视图键族（§12.10-②）在 legacyKeys 声明——两者并存。
    invalidates: [{ resource: varSetsResource.name }],
    legacyKeys: varSetSubviewKeys,
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
      if (!form.num.trim()) return { error: "整数不能为空" };
      const n = Number(form.num);
      if (!Number.isInteger(n)) return { error: "不是合法的整数" };
      if (Math.abs(n) > SAFE_INTEGER_LIMIT) {
        return { error: "超出安全整数范围（±2^53−1）" };
      }
      return { value: { integer_value: n } };
    }
    case "float": {
      if (!form.num.trim()) return { error: "数值不能为空" };
      const n = Number(form.num);
      if (!Number.isFinite(n)) return { error: "不是合法的数值" };
      return { value: { float_value: n } };
    }
    case "boolean":
      return { value: { bool_value: form.bool === "true" } };
    case "json": {
      if (!form.json.trim()) return { error: "JSON 不能为空" };
      try {
        JSON.parse(form.json);
      } catch (e) {
        return {
          error: `JSON 格式不合法：${e instanceof Error ? e.message : String(e)}`,
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
      if (!value) return { status: "rejected", message: built.error ?? "值不合法" };
      return runCommandMapped(() =>
        updateVar(varSetId, key, { value, description: input.description }, SKIP)
      );
    },
    // var 写 bump 集合 revision/var_count/footprint——kit 列表键显式声明失效；
    // 存量子视图键族（§12.10-②）在 legacyKeys 声明——两者并存。
    invalidates: [{ resource: varSetsResource.name }],
    legacyKeys: varSetSubviewKeys,
  };
}

// —— 列表 ——

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
  // 存量键失效桥（§12.10-②，宽前缀）：集合级动词 ok 后除 kit 键外失效存量
  // ["runtime-var-sets"] 前缀键（覆盖 ["runtime-var-sets", projectId] 形）；
  // var 级动词的子视图键族在各自 descriptor 内声明（见上）。
  legacyKeys: () => [["runtime-var-sets"]],
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
        if (!value) return { status: "rejected", message: built.error ?? "值不合法" };
        return runCommandMapped(() =>
          createVar(
            input.varSetId,
            { key: input.key, value, description: input.description },
            SKIP
          )
        );
      },
      // var 写 bump revision/var_count/footprint——存量子视图键族全桥（§12.10-②）
      legacyKeys: varSetSubviewKeys,
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
      legacyKeys: varSetSubviewKeys,
    },
    // 回滚到目标版本（custom verb）；目标=当前版本 → 400 InvalidArgument（D12）
    // → rejected；窗口淘汰 → 404（失败通道，describeApiError）
    rollback: {
      run: async (input: {
        varSetId: string;
        targetRevision: number;
      }): Promise<CommandResult<RuntimeVarVersion>> =>
        runCommandMapped(() => rollback(input.varSetId, input.targetRevision, SKIP)),
      legacyKeys: varSetSubviewKeys,
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
