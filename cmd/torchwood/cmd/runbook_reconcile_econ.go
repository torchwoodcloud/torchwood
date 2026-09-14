package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// runbook 引擎阶段 D · 经济三域动词（docs/design/runbook.md §2.4 动词表的
// bucket / asset def / leaderboard board 三行与「asset def 归档语义」段，D10/D12）：
//   - bucket 按 `name` 寻址（ID 是服务端 UUID，读取路径 ListBuckets 按 name
//     过滤解析 id：0 命中 = 不存在、多命中 = fail 人工裁决）；
//   - asset def 按 `code` 寻址（def_id 服务端生成，读取路径 ListAssetDefs 按
//     code 反查；服务端面恒含归档行）；create 的 D10 复活分支 = 遇同 code 且
//     status=archived、配置相等（除 status）→ UpdateAssetDef(status=active)；
//   - board 按 `id` 寻址且 create 直传服务端原生幂等 provisioning（唯一
//     「比较在服务端」的资源域，引擎不先读后建）。
//
// 动词 schema 静态表与决策分发的登记点在 runbook_reconcile.go 的
// runbookVerbSpecs / action()；本文件只放三域的读路径、D12 比较白名单与
// reconcile 实现——全部经 runbookCaller 抽象，不 import genproto。

// ---------------------------------------------------------------------------
// RPC 方法全名
// ---------------------------------------------------------------------------

func runbookStorageMethod(name string) string {
	return "/torchwood.server.v1.StorageService/" + name
}

func runbookAssetsMethod(name string) string {
	return "/torchwood.server.v1.AssetsService/" + name
}

func runbookLeaderboardsMethod(name string) string {
	return "/torchwood.server.v1.LeaderboardsService/" + name
}

// ---------------------------------------------------------------------------
// 分页列表（bucket / asset def 的读取路径）
// ---------------------------------------------------------------------------

const (
	// runbookListPageSize 是引擎拉列表的页大小（bucket 缺省 25、asset def 上限
	// 100，请求 100 让两域都按最大页返回）。
	runbookListPageSize = 100
	// runbookListMaxPages 是分页跟随的防御性页数上限（next_page_token 永不
	// 排干的服务端异常下 fail 而非死循环）。
	runbookListMaxPages = 1000
)

// listAllPages 拉全量列表（跟随 meta.next_page_token 到排干）。listKey 是
// 响应里的列表键（"buckets" / "defs"）；两域的 List 都不支持服务端过滤
// （buckets 携带 queries 即 InvalidArgument），name/code 过滤在引擎侧做。
func (r *runbookReconciler) listAllPages(method, listKey string) ([]map[string]any, error) {
	var out []map[string]any
	req := map[string]any{"page_size": runbookListPageSize}
	for page := 0; page < runbookListMaxPages; page++ {
		resp, err := r.caller(method, req)
		if err != nil {
			return nil, err
		}
		for _, item := range runbookAnyList(resp[listKey]) {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		meta, _ := resp["meta"].(map[string]any)
		token := runbookGotStr(meta, "nextPageToken")
		if token == "" {
			return out, nil
		}
		req = map[string]any{"page_size": runbookListPageSize, "page_token": token}
	}
	return nil, fmt.Errorf("%s: listing did not drain after %d pages (next_page_token never empty)", shortRunbookMethod(method), runbookListMaxPages)
}

// ---------------------------------------------------------------------------
// D12/D20 比较白名单（经济三域）
// ---------------------------------------------------------------------------

// runbookKeySet 报告动作体/读回里键是否存在（proto3 optional presence 与
// protojson 省略语义的统一取值口）。
func runbookKeySet(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func sortRunbookStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// compareRunbookBucket：D12 白名单 bucket = Create/Update 可写字段全集
// （name/public/permissions）。permissions 排序归一（D20：顺序无语义）；bucket
// 无服务端展开与缺省赋权（permissions 原样落库，A8 后读路径不用于鉴权），故
// 不做 write 镜像展开、也不跳过缺省比较——声明侧空与读回侧空天然相等。
func compareRunbookBucket(want, got map[string]any) []runbookFieldDiff {
	var diffs []runbookFieldDiff
	if w, g := runbookWantStr(want, "name"), runbookGotStr(got, "name"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "name", Want: w, Got: g})
	}
	if w, g := runbookWantBool(want, "public"), runbookGotBool(got, "public"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "public", Want: w, Got: g})
	}
	w, g := sortRunbookStrings(runbookWantStrList(want, "permissions")), sortRunbookStrings(runbookGotStrList(got, "permissions"))
	if !runbookStrSlicesEqual(w, g) {
		diffs = append(diffs, runbookFieldDiff{Field: "permissions", Want: w, Got: g})
	}
	return diffs
}

// compareRunbookAssetDef：D12 白名单 asset def = name/class/decimals/max_quantity/
// expires_in/tradable/unique_per_owner/upgradeable/metadata（status 单独走 D10，
// 不参与比较）。want 侧镜像 app CreateDef 的类别矩阵 forcing（currency →
// unique_per_owner=true；entitlement → unique_per_owner=true 且 tradable=false）：
// 不镜像则声明省略这些键时读回必为 true/false，双跑误报漂移。
func compareRunbookAssetDef(want, got map[string]any) []runbookFieldDiff {
	var diffs []runbookFieldDiff
	if w, g := runbookWantStr(want, "name"), runbookGotStr(got, "name"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "name", Want: w, Got: g})
	}
	if w, g := runbookWantStr(want, "class"), runbookGotStr(got, "class"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "class", Want: w, Got: g})
	}
	if w, g := runbookWantInt(want, "decimals"), runbookGotInt(got, "decimals"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "decimals", Want: w, Got: g})
	}
	diffs = append(diffs, compareRunbookOptInt(want, got, "max_quantity", "maxQuantity")...)
	diffs = append(diffs, compareRunbookOptInt(want, got, "expires_in", "expiresIn")...)
	wTradable, wUnique := runbookWantBool(want, "tradable"), runbookWantBool(want, "unique_per_owner")
	switch runbookWantStr(want, "class") {
	case "currency":
		wUnique = true
	case "entitlement":
		wUnique, wTradable = true, false
	}
	if w, g := wTradable, runbookGotBool(got, "tradable"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "tradable", Want: w, Got: g})
	}
	if w, g := wUnique, runbookGotBool(got, "uniquePerOwner"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "unique_per_owner", Want: w, Got: g})
	}
	if w, g := runbookWantBool(want, "upgradeable"), runbookGotBool(got, "upgradeable"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "upgradeable", Want: w, Got: g})
	}
	diffs = append(diffs, compareRunbookStruct(want, got, "metadata")...)
	return diffs
}

// compareRunbookOptInt 比较 proto3 optional int64（max_quantity/expires_in）：
// YAML 键存在 = 设置，protojson 键省略 = 未设置；存在性或值不等出 diff
// （缺侧渲染为 <absent>）。want/got 分键：声明侧 snake_case、读回侧 camelCase。
func compareRunbookOptInt(want, got map[string]any, wantKey, gotKey string) []runbookFieldDiff {
	wSet, gSet := runbookKeySet(want, wantKey), runbookKeySet(got, gotKey)
	if !wSet && !gSet {
		return nil
	}
	var wVal, gVal any
	if wSet {
		wVal = runbookWantInt(want, wantKey)
	}
	if gSet {
		gVal = runbookGotInt(got, gotKey)
	}
	if wSet != gSet || wVal != gVal {
		return []runbookFieldDiff{{Field: wantKey, Want: wVal, Got: gVal}}
	}
	return nil
}

// compareRunbookStruct 比较 google.protobuf.Struct 字段（metadata）：JSON
// 规范形比较——encoding/json 对 map 键排序，int / json.Number 等数字形态
// （YAML int vs protojson UseNumber）序列化后同形即相等。
func compareRunbookStruct(want, got map[string]any, key string) []runbookFieldDiff {
	wSet, gSet := runbookKeySet(want, key), runbookKeySet(got, key)
	if !wSet && !gSet {
		return nil
	}
	wJSON, wErr := json.Marshal(want[key])
	gJSON, gErr := json.Marshal(got[key])
	if wErr == nil && gErr == nil && string(wJSON) == string(gJSON) {
		return nil
	}
	return []runbookFieldDiff{{Field: key, Want: want[key], Got: got[key]}}
}

// ---------------------------------------------------------------------------
// bucket（引用键 name，ID 服务端 UUID）
// ---------------------------------------------------------------------------

// findRunbookBucketsByName 经 ListBuckets 按 name 过滤（引擎侧过滤：storage
// 面不支持查询串）。
func (r *runbookReconciler) findBucketsByName(name string) ([]map[string]any, error) {
	buckets, err := r.listAllPages(runbookStorageMethod("ListBuckets"), "buckets")
	if err != nil {
		return nil, err
	}
	var matches []map[string]any
	for _, b := range buckets {
		if runbookGotStr(b, "name") == name {
			matches = append(matches, b)
		}
	}
	return matches, nil
}

func runbookAmbiguousBucketError(name string, matches []map[string]any) error {
	ids := make([]string, 0, len(matches))
	for _, b := range matches {
		ids = append(ids, runbookGotStr(b, "id"))
	}
	sort.Strings(ids)
	return fmt.Errorf("bucket name %q is ambiguous: %d buckets carry it (ids: %s) — resolve the duplicates manually and re-run", name, len(matches), strings.Join(ids, ", "))
}

// bucketByName 按 name 读唯一 bucket：0 命中 → (nil, nil)（按不存在处理）；
// 多命中 → fail 人工裁决（§5：重名是环境异常，引擎不猜）。
func (r *runbookReconciler) bucketByName(name string) (map[string]any, error) {
	matches, err := r.findBucketsByName(name)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, runbookAmbiguousBucketError(name, matches)
	}
}

// bucketIDByName 把 name 解析为 bucket id（update/delete 的寻址前置）；0 命中
// 归一为 NotFound 分类错误——与 update_collection 的服务端 NotFound 同渠道
// （rpcExitCode 的 40x→2 映射一致）。
func (r *runbookReconciler) bucketIDByName(name string) (string, error) {
	b, err := r.bucketByName(name)
	if err != nil {
		return "", err
	}
	if b == nil {
		return "", &runbookCallError{code: runbookCodeNotFound, err: fmt.Errorf("bucket named %q not found", name)}
	}
	return runbookGotStr(b, "id"), nil
}

func (r *runbookReconciler) createBucket(body map[string]any) (runbookActionOutcome, error) {
	name := runbookWantStr(body, "name")
	result, err := r.ensureCreate("bucket "+name,
		func() (map[string]any, error) { return r.bucketByName(name) },
		runbookVerbSpecs["create_bucket"].rpc, body,
		func(got map[string]any) []runbookFieldDiff { return compareRunbookBucket(body, got) })
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("create_bucket", name, result), nil
}

// updateBucket 用解析出的 id 调 UpdateBucket；体直传（YAML 键存在性 =
// optional presence，§2.4 update 通则：天然幂等，不做比较）。YAML `name`
// 既是引用键也是 UpdateBucketRequest.name 本身——透传为同值自设（no-op），
// 重命名不在本动词表达（按名寻址的幂等性要求地址稳定，改名走新 step 的
// create+delete 组合）。
func (r *runbookReconciler) updateBucket(body map[string]any) (runbookActionOutcome, error) {
	name := runbookWantStr(body, "name")
	id, err := r.bucketIDByName(name)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	req := make(map[string]any, len(body)+1)
	for k, v := range body {
		req[k] = v
	}
	req["id"] = id
	if _, err := r.write(runbookVerbSpecs["update_bucket"].rpc, req); err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("update_bucket", name, r.done("updated")), nil
}

func (r *runbookReconciler) deleteBucket(body map[string]any) (runbookActionOutcome, error) {
	name := runbookWantStr(body, "name")
	result, err := r.ensureDeleteByID(
		func() (map[string]any, error) { return r.bucketByName(name) },
		runbookVerbSpecs["delete_bucket"].rpc,
		func(got map[string]any) map[string]any { return map[string]any{"id": runbookGotStr(got, "id")} })
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("delete_bucket", name, result), nil
}

// ensureDeleteByID 覆盖「引用键非 ID」资源域的 delete 同构形状（ensureDelete
// 的变体：Delete 请求体携带的是 read 解析出的服务端 ID 而非 YAML 引用键）：
// 不存在 → skip；执行撞 NotFound 一律按成功（§2.3）。
func (r *runbookReconciler) ensureDeleteByID(
	read func() (map[string]any, error),
	rpc string,
	req func(got map[string]any) map[string]any,
) (string, error) {
	got, err := read()
	if err != nil {
		return "", err
	}
	if got == nil {
		return "skipped", nil
	}
	if _, err := r.write(rpc, req(got)); err != nil {
		if isRunbookCode(err, runbookCodeNotFound) {
			return "skipped", nil
		}
		return "", err
	}
	return r.done("deleted"), nil
}

// ---------------------------------------------------------------------------
// asset def（引用键 code，def_id 服务端生成）
// ---------------------------------------------------------------------------

// assetDefByCode 经 ListAssetDefs 按 code 反查（服务端面恒含归档行；
// UNIQUE (project_id, code) 不分 status，归档行占位 code）。多命中是异常态
// （约束被破坏），fail 不猜。
func (r *runbookReconciler) assetDefByCode(code string) (map[string]any, error) {
	defs, err := r.listAllPages(runbookAssetsMethod("ListAssetDefs"), "defs")
	if err != nil {
		return nil, err
	}
	var match map[string]any
	for _, d := range defs {
		if runbookGotStr(d, "code") == code {
			if match != nil {
				return nil, fmt.Errorf("asset def code %q resolved to multiple defs — resolve manually and re-run", code)
			}
			match = d
		}
	}
	return match, nil
}

// ensureAssetDef 是 create_asset_def 的 reconcile（含 D10 复活分支）：
//   - 不存在 → CreateAssetDef（撞 AlreadyExists = TOCTOU / 归档行占位 →
//     重读重判一次）；
//   - 存在且配置相等（D12 白名单，status 除外）→ active 则 skip；archived 则
//     UpdateAssetDef(status=active) 复活（归档是逻辑删除，复活是其逆——
//     down(归档)→再 up 若不复活就死锁，D10）；
//   - 存在但不等 → fail 带 diff（D6：永不自动 update）。
func (r *runbookReconciler) ensureAssetDef(body map[string]any) (string, error) {
	code := runbookWantStr(body, "code")
	resource := "asset def " + code
	for attempt := 0; ; attempt++ {
		def, err := r.assetDefByCode(code)
		if err != nil {
			return "", err
		}
		if def == nil {
			if _, err := r.write(runbookVerbSpecs["create_asset_def"].rpc, body); err != nil {
				if isRunbookCode(err, runbookCodeAlreadyExists) && attempt == 0 {
					continue // TOCTOU：并发首建或归档行占位，重读现状再判
				}
				return "", err
			}
			return r.done("created"), nil
		}
		if diffs := compareRunbookAssetDef(body, def); len(diffs) > 0 {
			return "", &runbookDriftError{resource: resource, diffs: diffs}
		}
		if runbookGotStr(def, "status") == "archived" {
			if _, err := r.write(runbookVerbSpecs["update_asset_def"].rpc, map[string]any{
				"def_id": runbookGotStr(def, "id"),
				"status": "active",
			}); err != nil {
				return "", err
			}
			return r.done("restored"), nil
		}
		return "skipped", nil
	}
}

func (r *runbookReconciler) createAssetDef(body map[string]any) (runbookActionOutcome, error) {
	code := runbookWantStr(body, "code")
	result, err := r.ensureAssetDef(body)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("create_asset_def", code, result), nil
}

// updateAssetDef 用 code 反查出的 def_id 调 UpdateAssetDef；体直传但剥离
// 引用键 code（UpdateAssetDefRequest 无该字段，class/code 不可变），YAML 键
// 存在性 = proto3 optional presence。
func (r *runbookReconciler) updateAssetDef(body map[string]any) (runbookActionOutcome, error) {
	code := runbookWantStr(body, "code")
	def, err := r.assetDefByCode(code)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	if def == nil {
		return runbookActionOutcome{}, &runbookCallError{code: runbookCodeNotFound, err: fmt.Errorf("asset def %q not found", code)}
	}
	req := make(map[string]any, len(body)+1)
	for k, v := range body {
		req[k] = v
	}
	delete(req, "code")
	req["def_id"] = runbookGotStr(def, "id")
	if _, err := r.write(runbookVerbSpecs["update_asset_def"].rpc, req); err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("update_asset_def", code, r.done("updated")), nil
}

// deleteAssetDef = DeleteAssetDef（服务端语义为归档软删，流水与 holdings 随
// def 保留正是 soft 语义的正确行为）。已是归档态 = 已是目标态 → skip；
// 不存在 → skip（重建靠 D10 复活，down→up 循环闭合）。
func (r *runbookReconciler) deleteAssetDef(body map[string]any) (runbookActionOutcome, error) {
	code := runbookWantStr(body, "code")
	result, err := r.ensureDeleteByID(
		func() (map[string]any, error) {
			def, err := r.assetDefByCode(code)
			if err != nil {
				return nil, err
			}
			if def != nil && runbookGotStr(def, "status") == "archived" {
				return nil, nil // 已归档：目标态已达成
			}
			return def, nil
		},
		runbookVerbSpecs["delete_asset_def"].rpc,
		func(got map[string]any) map[string]any { return map[string]any{"def_id": runbookGotStr(got, "id")} })
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("delete_asset_def", code, result), nil
}

// ---------------------------------------------------------------------------
// leaderboard board（引用键 id；create 直传服务端原生幂等 provisioning）
// ---------------------------------------------------------------------------

// createLeaderboardBoard 直传 CreateLeaderboardBoard（内部走服务端
// CreateBoardProvisioning）：相等（2xx，含重放）→ 引擎侧收敛为完成；不等 →
// AlreadyExists，服务端返回的字段 diff 经错误链原样透传（D12：board 是唯一
// 「比较在服务端」的资源域，引擎不先读后建、不镜像比较）。2xx 无法区分
// 「真新建」与「重放相等」——服务端不区分，报告统一为 created（语义 =
// 声明态已成立）。
func (r *runbookReconciler) createLeaderboardBoard(body map[string]any) (runbookActionOutcome, error) {
	boardID := runbookWantStr(body, "id")
	if _, err := r.write(runbookVerbSpecs["create_leaderboard_board"].rpc, body); err != nil {
		return runbookActionOutcome{}, fmt.Errorf("board provisioning rejected the declaration (server-side diff follows): %w", err)
	}
	return runbookSimpleOutcome("create_leaderboard_board", boardID, r.done("created")), nil
}

// updateLeaderboardBoard 按字段 presence 直传 UpdateLeaderboardBoard（写 =
// YAML 键存在；update 天然幂等，reconcile 直接执行不做比较——§2.4 update
// 通则）。board_id 是用户自选 ID，无需解析。
func (r *runbookReconciler) updateLeaderboardBoard(body map[string]any) (runbookActionOutcome, error) {
	boardID := runbookWantStr(body, "board_id")
	if _, err := r.write(runbookVerbSpecs["update_leaderboard_board"].rpc, body); err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("update_leaderboard_board", boardID, r.done("updated")), nil
}
