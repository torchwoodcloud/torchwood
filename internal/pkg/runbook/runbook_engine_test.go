package runbook

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// ---------------------------------------------------------------------------
// 假 caller：一个内存版 server（状态面 + DocumentDB DDL 面 + 经济三域）
// ---------------------------------------------------------------------------
//
// SDK 的 bufconn 设施只起 fake HealthService，不起真 server；而真 server 运行
// 时装配在 cmd/server/internal/runtime（internal 可见性使 cmd/torchwood 无法
// import），CLI 侧做真 server 端到端在结构上不可行。集成语义由本假 caller 的
// 全链路单测承担：它忠实镜像服务端的关键行为——Record 的 CAS（expect != top →
// FailedPrecondition；唯一约束 → AlreadyExists）、Delete 的顶版校验、权限的
// write 展开与缺省赋权、hnsw metric 的 COSINE 归一、protojson 零值省略；阶段 D
// 补齐经济三域——bucket 的 name 寻址（无重名约束、permissions 原样落库）、
// asset def 的 code 反查（UNIQUE 不分 status、类别矩阵 forcing、归档/复活）、
// board 的原生幂等 provisioning（缺省归一 + 相等 200 / 不等 AlreadyExists 附
// diff，消息格式与 internal/app/leaderboards/provision.go 一致）。

// fakeRunbookCall 记录一次引擎发起的调用（断言调用序/无资源动作等）。
type fakeRunbookCall struct {
	Method string
	Req    map[string]any
}

type fakeRunbookWorld struct {
	mu          sync.Mutex
	steps       []runbookStateStep
	databases   map[string]map[string]any
	collections map[string]map[string]any // key: db + "\x00" + collection
	buckets     map[string]map[string]any // key: bucket id（ID 服务端 UUID 生成）
	assetDefs   map[string]map[string]any // key: def code（UNIQUE (project_id, code)，含归档行）
	boards      map[string]map[string]any // key: board id（缺省归一后的形态，camelCase 键）
	calls       []fakeRunbookCall
	intercept   func(w *fakeRunbookWorld, method string, req map[string]any) error
}

func newFakeRunbookWorld() *fakeRunbookWorld {
	return &fakeRunbookWorld{
		databases:   map[string]map[string]any{},
		collections: map[string]map[string]any{},
		buckets:     map[string]map[string]any{},
		assetDefs:   map[string]map[string]any{},
		boards:      map[string]map[string]any{},
	}
}

func (w *fakeRunbookWorld) top() int64 {
	if len(w.steps) == 0 {
		return 0
	}
	return w.steps[len(w.steps)-1].Version
}

func (w *fakeRunbookWorld) collectionKey(db, coll string) string {
	return db + "\x00" + coll
}

// seedCollection 直接置入一个集合（arrange 并发/漂移场景用）。
func (w *fakeRunbookWorld) seedCollection(db, id, name string, perms []any, docSec bool, attrs, idxs []any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.collections[w.collectionKey(db, id)] = map[string]any{
		"id": id, "databaseId": db, "name": name,
		"permissions":      fakeExpandPermissions(perms),
		"documentSecurity": docSec,
		"disabled":         false,
		"attributes":       attrs,
		"indexes":          idxs,
	}
}

func fakeExpandPermissions(items []any) []any {
	out := []any{}
	for _, item := range items {
		s, _ := item.(string)
		typ, role, ok := strings.Cut(strings.TrimSpace(s), ":")
		if !ok || typ == "" || role == "" {
			continue
		}
		if typ == "write" {
			out = append(out, "create:"+role, "update:"+role, "delete:"+role)
			continue
		}
		out = append(out, s)
	}
	// 服务端对空声明赋缺省权限集（ParsePermissionStrings([]) → defaults）。
	if len(items) == 0 {
		for _, d := range []string{"create:keys", "create:users", "delete:keys", "delete:users", "read:keys", "update:keys", "update:users"} {
			out = append(out, d)
		}
		return out
	}
	sort.Slice(out, func(i, j int) bool { return out[i].(string) < out[j].(string) })
	return out
}

func fakeRunbookErr(code, msg string) error {
	return &CallError{code: code, err: fmt.Errorf("rpc failed: %s", msg)}
}

// fakePrune 模拟 protojson 的零值省略（空串/false/0/空列表不出现在响应里）。
func fakePrune(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			if t == "" {
				continue
			}
		case bool:
			if !t {
				continue
			}
		case int64:
			if t == 0 {
				continue
			}
		case int:
			if t == 0 {
				continue
			}
		case []any:
			if len(t) == 0 {
				continue
			}
		}
		out[k] = v
	}
	return out
}

func fakeReqStr(req map[string]any, k string) string { s, _ := req[k].(string); return s }

func fakeReqInt(req map[string]any, k string) int64 {
	switch t := req[k].(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func fakeReqBool(req map[string]any, k string) bool { b, _ := req[k].(bool); return b }

func fakeReqStrList(req map[string]any, k string) []any { l, _ := req[k].([]any); return l }

func (w *fakeRunbookWorld) caller(method string, req map[string]any) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, fakeRunbookCall{Method: method, Req: req})
	// intercept 在持有 w.mu 的状态下回调：并发注入器只能直接改内部字段，
	// 不得再调带锁的 seed 辅助方法（否则自锁死锁）。
	if w.intercept != nil {
		if err := w.intercept(w, method, req); err != nil {
			return nil, err
		}
	}
	switch method {
	case runbookMethodGetState:
		steps := make([]any, 0, len(w.steps))
		for _, s := range w.steps {
			steps = append(steps, map[string]any{
				"version": s.Version, "name": s.Name, "checksum": s.Checksum,
				"appliedAt": "2026-09-14T00:00:00Z",
			})
		}
		return map[string]any{"steps": steps}, nil

	case runbookMethodRecord:
		version, expect := fakeReqInt(req, "version"), fakeReqInt(req, "expect_prev_version")
		if expect != w.top() {
			return nil, fakeRunbookErr(runbookCodeFailedPrecondition,
				fmt.Sprintf("expected previous version %d, current top is %d", expect, w.top()))
		}
		if version <= w.top() { // UNIQUE (project_id, runbook, version) 兜底
			return nil, fakeRunbookErr(runbookCodeAlreadyExists, "step already recorded")
		}
		w.steps = append(w.steps, runbookStateStep{
			Version: version, Name: fakeReqStr(req, "name"), Checksum: fakeReqStr(req, "checksum"),
		})
		return map[string]any{"currentVersion": version}, nil

	case runbookMethodDelete:
		if len(w.steps) == 0 {
			return nil, fakeRunbookErr(runbookCodeNotFound, "no recorded steps")
		}
		version := fakeReqInt(req, "version")
		if version != w.top() {
			return nil, fakeRunbookErr(runbookCodeFailedPrecondition,
				fmt.Sprintf("can only delete the top version (top %d, requested %d)", w.top(), version))
		}
		w.steps = w.steps[:len(w.steps)-1]
		return map[string]any{}, nil

	case runbookDBMethod("GetDatabase"):
		if db, ok := w.databases[fakeReqStr(req, "id")]; ok {
			return fakePrune(db), nil
		}
		return nil, fakeRunbookErr(runbookCodeNotFound, "database not found")

	case runbookDBMethod("CreateDatabase"):
		id, name := fakeReqStr(req, "id"), fakeReqStr(req, "name")
		if _, ok := w.databases[id]; ok {
			return nil, fakeRunbookErr(runbookCodeAlreadyExists, "database already exists")
		}
		w.databases[id] = map[string]any{"id": id, "name": name}
		return fakePrune(w.databases[id]), nil

	case runbookDBMethod("DeleteDatabase"):
		id := fakeReqStr(req, "id")
		if _, ok := w.databases[id]; !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "database not found")
		}
		delete(w.databases, id)
		// 级联：业务库删除时其下集合一并消失。
		for key := range w.collections {
			if strings.HasPrefix(key, id+"\x00") {
				delete(w.collections, key)
			}
		}
		return map[string]any{}, nil

	case runbookDBMethod("GetCollection"):
		if coll, ok := w.collections[w.collectionKey(fakeReqStr(req, "database_id"), fakeReqStr(req, "collection_id"))]; ok {
			return fakePrune(coll), nil
		}
		return nil, fakeRunbookErr(runbookCodeNotFound, "collection not found")

	case runbookDBMethod("CreateCollection"):
		db, id := fakeReqStr(req, "database_id"), fakeReqStr(req, "id")
		if _, ok := w.databases[db]; !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "database not found")
		}
		key := w.collectionKey(db, id)
		if _, ok := w.collections[key]; ok {
			return nil, fakeRunbookErr(runbookCodeAlreadyExists, "collection already exists")
		}
		coll := map[string]any{
			"id": id, "databaseId": db, "name": fakeReqStr(req, "name"),
			"permissions":      fakeExpandPermissions(fakeReqStrList(req, "permissions")),
			"documentSecurity": fakeReqBool(req, "document_security"),
			"disabled":         false,
			"attributes":       []any{},
			"indexes":          []any{},
		}
		w.collections[key] = coll
		return fakePrune(coll), nil

	case runbookDBMethod("UpdateCollection"):
		key := w.collectionKey(fakeReqStr(req, "database_id"), fakeReqStr(req, "collection_id"))
		coll, ok := w.collections[key]
		if !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "collection not found")
		}
		if v, ok := req["name"]; ok {
			coll["name"] = v
		}
		if v, ok := req["permissions"]; ok {
			if m, ok := v.(map[string]any); ok {
				coll["permissions"] = fakeExpandPermissions(fakeReqStrList(m, "values"))
			}
		}
		if v, ok := req["document_security"]; ok {
			coll["documentSecurity"] = v
		}
		if v, ok := req["disabled"]; ok {
			coll["disabled"] = v
		}
		return fakePrune(coll), nil

	case runbookDBMethod("DeleteCollection"):
		key := w.collectionKey(fakeReqStr(req, "database_id"), fakeReqStr(req, "collection_id"))
		if _, ok := w.collections[key]; !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "collection not found")
		}
		delete(w.collections, key)
		return map[string]any{}, nil

	case runbookDBMethod("CreateAttribute"):
		key := w.collectionKey(fakeReqStr(req, "database_id"), fakeReqStr(req, "collection_id"))
		coll, ok := w.collections[key]
		if !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "collection not found")
		}
		attrKey := fakeReqStr(req, "key")
		attrs := coll["attributes"].([]any)
		for i, item := range attrs {
			m := item.(map[string]any)
			if m["key"] == attrKey && m["status"] != "retired" {
				return nil, fakeRunbookErr(runbookCodeAlreadyExists, "attribute already exists")
			}
			if m["key"] == attrKey { // retired：物理列已删，覆盖重建
				attrs[i] = fakeBuildAttribute(req)
				return fakePrune(attrs[i].(map[string]any)), nil
			}
		}
		coll["attributes"] = append(attrs, fakeBuildAttribute(req))
		return fakePrune(fakeBuildAttribute(req)), nil

	case runbookDBMethod("DeleteAttribute"):
		key := w.collectionKey(fakeReqStr(req, "database_id"), fakeReqStr(req, "collection_id"))
		coll, ok := w.collections[key]
		if !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "collection not found")
		}
		attrKey := fakeReqStr(req, "key")
		for _, item := range coll["attributes"].([]any) {
			m := item.(map[string]any)
			if m["key"] == attrKey {
				m["status"] = "deprecated" // 软删两段的段一
				return map[string]any{}, nil
			}
		}
		return nil, fakeRunbookErr(runbookCodeNotFound, "attribute not found")

	case runbookDBMethod("RestoreAttribute"):
		key := w.collectionKey(fakeReqStr(req, "database_id"), fakeReqStr(req, "collection_id"))
		coll, ok := w.collections[key]
		if !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "collection not found")
		}
		attrKey := fakeReqStr(req, "key")
		for _, item := range coll["attributes"].([]any) {
			m := item.(map[string]any)
			if m["key"] == attrKey {
				delete(m, "status") // → active（protojson 省略）
				return map[string]any{}, nil
			}
		}
		return nil, fakeRunbookErr(runbookCodeNotFound, "attribute not found")

	case runbookDBMethod("CreateIndex"):
		key := w.collectionKey(fakeReqStr(req, "database_id"), fakeReqStr(req, "collection_id"))
		coll, ok := w.collections[key]
		if !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "collection not found")
		}
		idxID := fakeReqStr(req, "id")
		for _, item := range coll["indexes"].([]any) {
			if item.(map[string]any)["id"] == idxID {
				return nil, fakeRunbookErr(runbookCodeAlreadyExists, "index already exists")
			}
		}
		idx := map[string]any{
			"id": idxID, "type": fakeReqStr(req, "type"),
			"attributes": fakeReqStrList(req, "attributes"),
			"orders":     fakeReqStrList(req, "orders"),
		}
		// hnsw metric 归一（servergrpc/databases.go：大写 + 缺省 COSINE）。
		if strings.EqualFold(fakeReqStr(req, "type"), "hnsw") {
			metric := strings.ToUpper(fakeReqStr(req, "distance_metric"))
			if metric == "" {
				metric = "COSINE"
			}
			idx["distanceMetric"] = metric
		}
		coll["indexes"] = append(coll["indexes"].([]any), idx)
		return fakePrune(idx), nil

	case runbookDBMethod("DeleteIndex"):
		key := w.collectionKey(fakeReqStr(req, "database_id"), fakeReqStr(req, "collection_id"))
		coll, ok := w.collections[key]
		if !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "collection not found")
		}
		idxID := fakeReqStr(req, "index_id")
		idxs := coll["indexes"].([]any)
		for i, item := range idxs {
			if item.(map[string]any)["id"] == idxID {
				coll["indexes"] = append(idxs[:i:i], idxs[i+1:]...)
				return map[string]any{}, nil
			}
		}
		return nil, fakeRunbookErr(runbookCodeNotFound, "index not found")

	// ---- storage buckets（镜像 app/storage + servergrpc/storage：无重名
	// 约束（Insert 不查 name）、permissions 原样落库、Update 按 id）。----

	case runbookStorageMethod("ListBuckets"):
		// 单页全量（页大小 ≤100 的请求语义下无 token；分页跟随逻辑由
		// ListAssetDefs 的「非空页恒带 token」行为覆盖）。
		buckets := make([]any, 0, len(w.buckets))
		for _, b := range w.buckets {
			buckets = append(buckets, fakePrune(b))
		}
		return map[string]any{"buckets": buckets, "meta": map[string]any{}}, nil

	case runbookStorageMethod("CreateBucket"):
		name := fakeReqStr(req, "name")
		if name == "" {
			return nil, fakeRunbookErr("InvalidArgument", "name is required")
		}
		id := fmt.Sprintf("bkt_%04d", len(w.buckets)+1)
		b := map[string]any{
			"id":          id,
			"name":        name,
			"permissions": append([]any{}, fakeReqStrList(req, "permissions")...),
			"public":      fakeReqBool(req, "public"),
		}
		w.buckets[id] = b
		return fakePrune(b), nil

	case runbookStorageMethod("UpdateBucket"):
		b, ok := w.buckets[fakeReqStr(req, "id")]
		if !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "bucket not found")
		}
		if v, ok := req["name"]; ok {
			b["name"] = v
		}
		if v, ok := req["public"]; ok {
			b["public"] = v
		}
		return fakePrune(b), nil

	case runbookStorageMethod("DeleteBucket"):
		// 镜像真实服务端：repo Delete 不做存在性校验，删不存在的 id 也成功。
		delete(w.buckets, fakeReqStr(req, "id"))
		return map[string]any{}, nil

	// ---- asset defs（镜像 app/assets + domain/assets：UNIQUE (project_id,
	// code) 不分 status、类别矩阵校验与 forcing、归档/复活、optional 按
	// presence 应用、非空页恒带 next_page_token）。----

	case runbookAssetsMethod("ListAssetDefs"):
		meta := map[string]any{}
		if fakeReqStr(req, "page_token") != "" {
			// 第二页：空页无 token（镜像「末页之后是空页」的终止信号）。
			return map[string]any{"defs": []any{}, "meta": meta}, nil
		}
		defs := make([]any, 0, len(w.assetDefs))
		for _, d := range w.assetDefs {
			defs = append(defs, fakePrune(d))
		}
		if len(defs) > 0 {
			// 镜像 servergrpc/assets.go：非空首页恒带 token。
			meta["nextPageToken"] = "defs-page-2"
		}
		return map[string]any{"defs": defs, "meta": meta}, nil

	case runbookAssetsMethod("CreateAssetDef"):
		code, name := fakeReqStr(req, "code"), fakeReqStr(req, "name")
		if code == "" || name == "" {
			return nil, fakeRunbookErr("InvalidArgument", "code and name are required")
		}
		class := fakeReqStr(req, "class")
		switch class {
		case "currency", "stack", "instance", "entitlement":
		default:
			return nil, fakeRunbookErr("InvalidArgument", fmt.Sprintf("assets: invalid class %q", class))
		}
		if _, ok := w.assetDefs[code]; ok {
			// UNIQUE (project_id, code) 不分 status：归档行同样占位撞键。
			return nil, fakeRunbookErr(runbookCodeAlreadyExists, "asset def already exists")
		}
		d := map[string]any{
			"id": "def_" + code, "code": code, "name": name, "class": class,
			"decimals":       fakeReqInt(req, "decimals"),
			"tradable":       fakeReqBool(req, "tradable"),
			"uniquePerOwner": fakeReqBool(req, "unique_per_owner"),
			"upgradeable":    fakeReqBool(req, "upgradeable"),
			"status":         "active",
		}
		if v, ok := req["max_quantity"]; ok {
			d["maxQuantity"] = v
		}
		if v, ok := req["expires_in"]; ok {
			d["expiresIn"] = v
		}
		if v, ok := req["metadata"]; ok {
			d["metadata"] = v
		}
		// 镜像 app CreateDef 的类别矩阵 forcing。
		switch class {
		case "currency":
			d["uniquePerOwner"] = true
		case "entitlement":
			d["uniquePerOwner"] = true
			d["tradable"] = false
		}
		w.assetDefs[code] = d
		return fakePrune(d), nil

	case runbookAssetsMethod("UpdateAssetDef"):
		d := w.findAssetDefByID(fakeReqStr(req, "def_id"))
		if d == nil {
			return nil, fakeRunbookErr(runbookCodeNotFound, "asset def not found")
		}
		if v, ok := req["name"]; ok {
			d["name"] = v
		}
		if v, ok := req["decimals"]; ok {
			d["decimals"] = v
		}
		if v, ok := req["max_quantity"]; ok {
			if fakeReqInt(req, "max_quantity") <= 0 {
				delete(d, "maxQuantity") // <=0 = ClearMax（servergrpc 映射）
			} else {
				d["maxQuantity"] = v
			}
		}
		if v, ok := req["expires_in"]; ok {
			if fakeReqInt(req, "expires_in") <= 0 {
				delete(d, "expiresIn")
			} else {
				d["expiresIn"] = v
			}
		}
		if v, ok := req["tradable"]; ok {
			d["tradable"] = v
		}
		if v, ok := req["unique_per_owner"]; ok {
			d["uniquePerOwner"] = v
		}
		if v, ok := req["upgradeable"]; ok {
			d["upgradeable"] = v
		}
		if v, ok := req["metadata"]; ok {
			d["metadata"] = v
		}
		if v, ok := req["status"]; ok {
			d["status"] = v
		}
		// 镜像 app UpdateDef 的 entitlement re-forcing。
		if d["class"] == "entitlement" {
			d["tradable"] = false
			d["uniquePerOwner"] = true
		}
		return fakePrune(d), nil

	case runbookAssetsMethod("DeleteAssetDef"):
		d := w.findAssetDefByID(fakeReqStr(req, "def_id"))
		if d == nil {
			return nil, fakeRunbookErr(runbookCodeNotFound, "asset def not found")
		}
		d["status"] = "archived" // DeleteDef = UpdateDef(status=archived)
		return map[string]any{}, nil

	// ---- leaderboard boards（镜像 CreateBoardProvisioning：缺省归一 →
	// 相等 200 / 不等 AlreadyExists 附 diff；消息格式对齐 provision.go）。----

	case runbookLeaderboardsMethod("CreateLeaderboardBoard"):
		boardID := fakeReqStr(req, "id")
		if boardID == "" {
			return nil, fakeRunbookErr("InvalidArgument", "id is required")
		}
		want := fakeBoardFromCreate(req)
		if got, ok := w.boards[boardID]; ok {
			if diff := fakeBoardConfigDiff(want, got); len(diff) > 0 {
				return nil, fakeRunbookErr(runbookCodeAlreadyExists,
					fmt.Sprintf("leaderboards: board %q already exists with different config: %s", boardID, strings.Join(diff, "; ")))
			}
			return fakePrune(got), nil
		}
		w.boards[boardID] = want
		return fakePrune(want), nil

	case runbookLeaderboardsMethod("UpdateLeaderboardBoard"):
		b, ok := w.boards[fakeReqStr(req, "board_id")]
		if !ok {
			return nil, fakeRunbookErr(runbookCodeNotFound, "board not found")
		}
		if v, ok := req["sort"]; ok {
			b["sort"] = v
		}
		if v, ok := req["tiebreak_order"]; ok {
			if s, _ := v.(string); s == "" {
				delete(b, "tiebreakOrder")
			} else {
				b["tiebreakOrder"] = v
			}
		}
		if fakeReqBool(req, "clear_tiebreak") {
			delete(b, "tiebreakOrder")
		}
		if v, ok := req["tie_break"]; ok {
			b["tieBreak"] = v
		}
		if v, ok := req["period_kind"]; ok {
			b["periodKind"] = v
		}
		if v, ok := req["period_tz"]; ok {
			b["periodTz"] = v
		}
		if v, ok := req["policy"]; ok {
			b["policy"] = v
		}
		if v, ok := req["value_min"]; ok {
			b["valueMin"] = v
		}
		if v, ok := req["value_max"]; ok {
			b["valueMax"] = v
		}
		if fakeReqBool(req, "clear_value_bounds") {
			delete(b, "valueMin")
			delete(b, "valueMax")
		}
		if v, ok := req["client_submit"]; ok {
			b["clientSubmit"] = v
		}
		if v, ok := req["per_subject_submit_limit"]; ok {
			b["perSubjectSubmitLimit"] = v
		}
		if v, ok := req["retention_periods"]; ok {
			b["retentionPeriods"] = v
		}
		if v, ok := req["subject_kind"]; ok {
			b["subjectKind"] = v
		}
		return fakePrune(b), nil
	}
	return nil, fmt.Errorf("fake world: unexpected method %s", method)
}

// findAssetDefByID 按 def_id 扫描（fake 以 code 为键存 def）。
func (w *fakeRunbookWorld) findAssetDefByID(defID string) map[string]any {
	for _, d := range w.assetDefs {
		if d["id"] == defID {
			return d
		}
	}
	return nil
}

// fakeBoardFromCreate 构造缺省归一后的 board 形态（applyCreateDefaults 镜像：
// sort=desc / tie_break=parallel / policy=best / period_kind=none /
// per_subject_submit_limit=100 / subject_kind=user；tiebreak_order 空串 = 未声明）。
func fakeBoardFromCreate(req map[string]any) map[string]any {
	b := map[string]any{
		"id":                    fakeReqStr(req, "id"),
		"sort":                  fakeReqStr(req, "sort"),
		"tieBreak":              fakeReqStr(req, "tie_break"),
		"periodKind":            fakeReqStr(req, "period_kind"),
		"periodTz":              fakeReqStr(req, "period_tz"),
		"policy":                fakeReqStr(req, "policy"),
		"clientSubmit":          fakeReqBool(req, "client_submit"),
		"perSubjectSubmitLimit": fakeReqInt(req, "per_subject_submit_limit"),
		"retentionPeriods":      fakeReqInt(req, "retention_periods"),
		"subjectKind":           fakeReqStr(req, "subject_kind"),
	}
	if b["sort"] == "" {
		b["sort"] = "desc"
	}
	if b["tieBreak"] == "" {
		b["tieBreak"] = "parallel"
	}
	if b["policy"] == "" {
		b["policy"] = "best"
	}
	if b["periodKind"] == "" {
		b["periodKind"] = "none"
	}
	if b["perSubjectSubmitLimit"] == int64(0) {
		b["perSubjectSubmitLimit"] = int64(100)
	}
	if b["subjectKind"] == "" {
		b["subjectKind"] = "user"
	}
	if v := fakeReqStr(req, "tiebreak_order"); v != "" {
		b["tiebreakOrder"] = v
	}
	if v, ok := req["value_min"]; ok {
		b["valueMin"] = v
	}
	if v, ok := req["value_max"]; ok {
		b["valueMax"] = v
	}
	return b
}

// fakeBoardConfigDiff 镜像 boardConfigDiff 的比较集与消息格式（proto 字段名，
// requested/existing + <unset>）。
func fakeBoardConfigDiff(want, got map[string]any) []string {
	var diffs []string
	str := func(key, wantKey, gotKey string) {
		w, g := want[wantKey], got[gotKey]
		wS, _ := w.(string)
		gS, _ := g.(string)
		if wS != gS {
			diffs = append(diffs, fmt.Sprintf("%s: requested %s, existing %s", key, fakeBoardRender(w), fakeBoardRender(g)))
		}
	}
	str("sort", "sort", "sort")
	str("tie_break", "tieBreak", "tieBreak")
	str("period_kind", "periodKind", "periodKind")
	str("period_tz", "periodTz", "periodTz")
	str("policy", "policy", "policy")
	str("subject_kind", "subjectKind", "subjectKind")
	opt := func(key, wantKey, gotKey string) {
		wSet, gSet := runbookKeySet(want, wantKey), runbookKeySet(got, gotKey)
		if !wSet && !gSet {
			return
		}
		wI, gI := fakeReqInt(want, wantKey), fakeReqInt(got, gotKey)
		if wSet != gSet || wI != gI {
			var wR, gR any
			if wSet {
				wR = wI
			}
			if gSet {
				gR = gI
			}
			diffs = append(diffs, fmt.Sprintf("%s: requested %s, existing %s", key, fakeBoardRender(wR), fakeBoardRender(gR)))
		}
	}
	opt("tiebreak_order", "tiebreakOrder", "tiebreakOrder")
	opt("value_min", "valueMin", "valueMin")
	opt("value_max", "valueMax", "valueMax")
	if w, g := want["clientSubmit"], got["clientSubmit"]; w != g {
		diffs = append(diffs, fmt.Sprintf("client_submit: requested %t, existing %t", w == true, g == true))
	}
	if w, g := fakeReqInt(want, "perSubjectSubmitLimit"), fakeReqInt(got, "perSubjectSubmitLimit"); w != g {
		diffs = append(diffs, fmt.Sprintf("per_subject_submit_limit: requested %d, existing %d", w, g))
	}
	if w, g := fakeReqInt(want, "retentionPeriods"), fakeReqInt(got, "retentionPeriods"); w != g {
		diffs = append(diffs, fmt.Sprintf("retention_periods: requested %d, existing %d", w, g))
	}
	return diffs
}

func fakeBoardRender(v any) string {
	if v == nil {
		return "<unset>"
	}
	return fmt.Sprintf("%v", v)
}

func fakeBuildAttribute(req map[string]any) map[string]any {
	return map[string]any{
		"key":          fakeReqStr(req, "key"),
		"type":         fakeReqStr(req, "type"),
		"size":         fakeReqInt(req, "size"),
		"required":     fakeReqBool(req, "required"),
		"array":        fakeReqBool(req, "array"),
		"defaultValue": fakeReqStr(req, "default_value"),
		"dims":         fakeReqInt(req, "dims"),
	}
}

// fakeFindAttribute 读假世界里的属性（测试断言用）。
func (w *fakeRunbookWorld) fakeFindAttribute(db, coll, key string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.collections[w.collectionKey(db, coll)]
	if !ok {
		return nil
	}
	for _, item := range c["attributes"].([]any) {
		m := item.(map[string]any)
		if m["key"] == key {
			return m
		}
	}
	return nil
}

// hasDDLCall 报告引擎是否发起过任何资源动作 RPC（forgive 不碰资源的断言）。
func (w *fakeRunbookWorld) hasDDLCall() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.calls {
		if strings.HasPrefix(c.Method, "/torchwood.server.v1.DatabasesService/") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// runbookEngineFixture 是引擎全链路测试的两步迁移：建库 → 建集合（复合动词）。
const runbookFixtureCreateDB = `up:
  - create_database:
      id: gold
      name: Gold Store
down:
  - delete_database:
      id: gold
`

const runbookFixtureCreateColl = `up:
  - create_collection:
      database_id: gold
      id: configs
      name: configs
      document_security: true
      permissions: ['read:users', 'write:keys']
      attributes:
        - { key: key, type: string, size: 64, required: true }
        - { key: value, type: json }
      indexes:
        - { id: key, type: unique, attributes: [key] }
down:
  - delete_collection:
      database_id: gold
      collection_id: configs
`

func writeEngineFixtureDir(t *testing.T) string {
	t.Helper()
	return writeRunbookDir(t, map[string]string{
		"000001_create_gold_db.yaml":      runbookFixtureCreateDB,
		"000002_create_configs_coll.yaml": runbookFixtureCreateColl,
	})
}

// runUpOnWorld 是 arrange/act 复合助手：对指定目录跑 up 并解码 summary。
func runUpOnWorld(t *testing.T, w *fakeRunbookWorld, dir string, mutate func(*RunOptions)) (UpSummary, error) {
	t.Helper()
	var out, errOut strings.Builder
	opts := RunOptions{Dir: dir}
	if mutate != nil {
		mutate(&opts)
	}
	err := RunUp(w.caller, opts, &out, &errOut)
	var summary UpSummary
	if out.Len() > 0 {
		require.NoError(t, decodeJSON(out.String(), &summary))
	}
	return summary, err
}

func runDownOnWorld(t *testing.T, w *fakeRunbookWorld, dir string, mutate func(*RunOptions)) (DownSummary, error) {
	t.Helper()
	var out, errOut strings.Builder
	opts := RunOptions{Dir: dir}
	if mutate != nil {
		mutate(&opts)
	}
	err := RunDown(w.caller, opts, &out, &errOut)
	var summary DownSummary
	if out.Len() > 0 {
		require.NoError(t, decodeJSON(out.String(), &summary))
	}
	return summary, err
}

// TestFetchRunbookStateProtojsonInt64AsString 锁定生产 RPC 链路的响应形状：
// protojson 把 int64 字段序列化为 JSON 字符串（"version": "3"），InvokeJSON
// 的响应 map 里 version 因此是 Go string 而非 number（真机 mlbridge runbook
// 首跑踩中）。引擎必须解析它，并拒绝非数字字符串。
func TestFetchRunbookStateProtojsonInt64AsString(t *testing.T) {
	caller := func(method string, req map[string]any) (map[string]any, error) {
		require.Equal(t, runbookMethodGetState, method)
		return map[string]any{"steps": []any{
			map[string]any{"version": "1", "name": "a", "checksum": strings.Repeat("a", 64)},
			map[string]any{"version": "12", "name": "b", "checksum": strings.Repeat("b", 64)},
		}}, nil
	}
	steps, err := fetchRunbookState(caller, runbookDefaultName)
	require.NoError(t, err)
	require.Len(t, steps, 2)
	require.Equal(t, int64(1), steps[0].Version)
	require.Equal(t, int64(12), steps[1].Version)

	bad := func(method string, req map[string]any) (map[string]any, error) {
		return map[string]any{"steps": []any{
			map[string]any{"version": "not-a-number", "name": "a", "checksum": strings.Repeat("a", 64)},
		}}, nil
	}
	_, err = fetchRunbookState(bad, runbookDefaultName)
	require.ErrorContains(t, err, "step version is not a number")
}

// ---------------------------------------------------------------------------
// gRPC code 名契约（错误分类的根基）
// ---------------------------------------------------------------------------

// TestRunbookCodeNameContract 钉死 codes.Code.String() 的 CamelCase 输出——
// 生产 caller 用 server.ErrorCode(err).String() 分类，引擎常量必须与其一致。
func TestRunbookCodeNameContract(t *testing.T) {
	require.Equal(t, runbookCodeNotFound, codes.NotFound.String())
	require.Equal(t, runbookCodeAlreadyExists, codes.AlreadyExists.String())
	require.Equal(t, runbookCodeFailedPrecondition, codes.FailedPrecondition.String())
}

// ---------------------------------------------------------------------------
// up：顺序应用 / 双跑幂等 / 对账 / CAS / dry-run / no-op step
// ---------------------------------------------------------------------------

func TestRunbookUpAppliesAndRecords(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)

	summary, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), summary.CurrentVersion)
	require.Len(t, summary.Applied, 2)

	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.steps, 2)
	require.Equal(t, int64(1), w.steps[0].Version)
	require.Equal(t, "create_gold_db", w.steps[0].Name)
	require.Equal(t, int64(2), w.steps[1].Version)
	require.Equal(t, "create_configs_coll", w.steps[1].Name)
	// 复合动词落地：集合 + 2 属性 + 1 索引。
	require.Contains(t, w.databases, "gold")
	coll := w.collections[w.collectionKey("gold", "configs")]
	require.NotNil(t, coll)
	require.Len(t, coll["attributes"].([]any), 2)
	require.Len(t, coll["indexes"].([]any), 1)
	require.True(t, coll["documentSecurity"].(bool))

	// summary 的声明动作粒度：step2 是一条 create_collection。
	require.Len(t, summary.Applied[1].Actions, 1)
	require.Equal(t, "create_collection", summary.Applied[1].Actions[0].Verb)
	require.Equal(t, "created", summary.Applied[1].Actions[0].Result)
	require.Equal(t, "gold.configs", summary.Applied[1].Actions[0].Target)
}

func TestRunbookUpIdempotentDoubleRun(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)

	_, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)

	summary, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Empty(t, summary.Applied, "双跑第二遍无事可做")
	require.Equal(t, int64(2), summary.CurrentVersion)
	w.mu.Lock()
	require.Len(t, w.steps, 2, "不重复记录 step")
	w.mu.Unlock()
	// 全部动作收敛为 skipped（drift-free）。
	rec := &runbookReconciler{caller: w.caller}
	out, err := rec.action(runbookAction{Verb: "create_database", Body: map[string]any{"id": "gold", "name": "Gold Store"}})
	require.NoError(t, err)
	require.Equal(t, "skipped", out.Report.Result)
}

func TestRunbookUpSkipsWhenConfigEqual(t *testing.T) {
	// 半应用重入：线上已有库+集合且配置相等（permissions 乱序 + write 展开等价），
	// up 应全部 skip 并记录版本。
	w := newFakeRunbookWorld()
	w.mu.Lock()
	w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold Store"}
	w.mu.Unlock()
	w.seedCollection("gold", "configs", "configs",
		[]any{"write:keys", "read:users"}, true,
		[]any{
			map[string]any{"key": "key", "type": "string", "size": int64(64), "required": true},
			map[string]any{"key": "value", "type": "json"},
		},
		[]any{map[string]any{"id": "key", "type": "unique", "attributes": []any{"key"}}})
	dir := writeRunbookDir(t, map[string]string{
		"000001_create_gold_db.yaml":      runbookFixtureCreateDB,
		"000002_create_configs_coll.yaml": runbookFixtureCreateColl,
	})

	summary, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), summary.CurrentVersion)
	require.Len(t, summary.Applied, 2)
	require.Equal(t, "skipped", summary.Applied[0].Actions[0].Result)
	require.Equal(t, "skipped", summary.Applied[1].Actions[0].Result)
}

func TestRunbookUpFailsOnDrift(t *testing.T) {
	// 线上库存在但 name 不等（D6：不等 fail 带 diff，永不自动 update）。
	w := newFakeRunbookWorld()
	w.mu.Lock()
	w.databases["gold"] = map[string]any{"id": "gold", "name": "Tampered Name"}
	w.mu.Unlock()
	dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})

	_, err := runUpOnWorld(t, w, dir, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "name")
	require.Contains(t, err.Error(), "Gold Store")
	require.Contains(t, err.Error(), "Tampered Name")
	require.Contains(t, err.Error(), "NEW runbook step")
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Empty(t, w.steps, "失败的 step 不记录版本（重跑即恢复，D7）")
}

func TestRunbookUpHistoryReconcileFails(t *testing.T) {
	t.Run("modified：已应用文件被改", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeEngineFixtureDir(t)
		_, err := runUpOnWorld(t, w, dir, nil)
		require.NoError(t, err)

		// 篡改 v1 文件内容（checksum 变化）。
		tampered := strings.Replace(runbookFixtureCreateDB, "Gold Store", "New Name", 1)
		require.NotEqual(t, runbookFixtureCreateDB, tampered)
		dir2 := writeRunbookDir(t, map[string]string{
			"000001_create_gold_db.yaml":      tampered,
			"000002_create_configs_coll.yaml": runbookFixtureCreateColl,
		})
		_, err = runUpOnWorld(t, w, dir2, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "checksum mismatch")
		require.Contains(t, err.Error(), "runbook forgive 1")
	})

	t.Run("orphan：服务端有本地无", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeEngineFixtureDir(t)
		_, err := runUpOnWorld(t, w, dir, nil)
		require.NoError(t, err)

		dir2 := writeRunbookDir(t, map[string]string{
			"000001_create_gold_db.yaml": runbookFixtureCreateDB, // v2 文件被删
		})
		_, err = runUpOnWorld(t, w, dir2, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "orphan")
		require.Contains(t, err.Error(), "runbook forgive 2")
	})
}

func TestRunbookUpCASFatalOnChecksumDivergence(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})

	// 并发赢家记录了同版本但不同 checksum（文件分叉）。
	w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
		if method == runbookMethodRecord {
			w.steps = append(w.steps, runbookStateStep{
				Version: 1, Name: "create_gold_db", Checksum: strings.Repeat("ab", 32),
			})
			return fakeRunbookErr(runbookCodeFailedPrecondition, "expected previous version 0, current top is 1")
		}
		return nil
	}
	_, err := runUpOnWorld(t, w, dir, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "diverges")
}

func TestRunbookUpCASConvergesOnIdenticalChecksum(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})

	files, err := loadRunbookDir(dir)
	require.NoError(t, err)
	localChecksum := files[0].Checksum

	// 并发赢家做了同样的事（同 checksum）。
	w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
		if method == runbookMethodRecord {
			w.steps = append(w.steps, runbookStateStep{
				Version: 1, Name: "create_gold_db", Checksum: localChecksum,
			})
			return fakeRunbookErr(runbookCodeFailedPrecondition, "expected previous version 0, current top is 1")
		}
		return nil
	}
	summary, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.CurrentVersion)
	require.Len(t, summary.Applied, 1)
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.steps, 1, "收敛为单赢家，不重复记录")
}

func TestRunbookUpDryRun(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)

	var errOut strings.Builder
	var out strings.Builder
	opts := RunOptions{Dir: dir, DryRun: true}
	err := RunUp(w.caller, opts, &out, &errOut)
	require.NoError(t, err, "dry-run 一律成功（§5 退出码契约）")

	var summary UpSummary
	require.NoError(t, decodeJSON(out.String(), &summary))
	require.True(t, summary.DryRun)
	require.Len(t, summary.Applied, 2)
	w.mu.Lock()
	require.Empty(t, w.steps, "不记录 step")
	require.Empty(t, w.databases, "不执行资源动作")
	w.mu.Unlock()
	// 读路径照常（计划要真实）。
	w.mu.Lock()
	require.NotEmpty(t, w.calls)
	w.mu.Unlock()

	// dry-run 后真跑仍完整落地。
	_, err = runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.steps, 2)
	require.Contains(t, w.databases, "gold")
}

func TestRunbookUpDryRunReportsWouldFail(t *testing.T) {
	w := newFakeRunbookWorld()
	w.mu.Lock()
	w.databases["gold"] = map[string]any{"id": "gold", "name": "Drifted"}
	w.mu.Unlock()
	dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})

	var out strings.Builder
	opts := RunOptions{Dir: dir, DryRun: true}
	err := RunUp(w.caller, opts, &out, &strings.Builder{})
	require.NoError(t, err, "dry-run 预报失败也退 0")
	var summary UpSummary
	require.NoError(t, decodeJSON(out.String(), &summary))
	require.NotEmpty(t, summary.WouldFail)
	require.Contains(t, summary.WouldFail, "name")
}

func TestRunbookUpNoOpStep(t *testing.T) {
	// 空 up 段合法（no-op step，对齐 golang-migrate 空迁移惯例）。
	w := newFakeRunbookWorld()
	dir := writeRunbookDir(t, map[string]string{"000001_noop.yaml": "up: []\ndown: []\n"})
	summary, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.CurrentVersion)
	require.Len(t, summary.Applied, 1)
	require.Empty(t, summary.Applied[0].Actions)
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.steps, 1)
}

// TestRunbookProgressLineContract：进度行进 stderr、summary 走 stdout（§2.1
// 输出契约），格式 = "[runbook] applying NNNNNN_name: verb(target) ... result"。
func TestRunbookProgressLineContract(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})

	var stdout, stderr strings.Builder
	require.NoError(t, RunUp(w.caller, RunOptions{Dir: dir}, &stdout, &stderr))

	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	require.Len(t, lines, 1)
	require.Regexp(t, `^\[runbook\] applying 000001_create_gold_db: create_database\(gold\) \.\.\. created$`, lines[0])

	var summary UpSummary
	require.NoError(t, decodeJSON(stdout.String(), &summary), "stdout 是单个 JSON summary")
	require.Equal(t, int64(1), summary.CurrentVersion)

	// --quiet 静默进度行，summary 仍在。
	var stdout2, stderr2 strings.Builder
	require.NoError(t, RunDown(w.caller, RunOptions{Dir: dir, All: true, Quiet: true}, &stdout2, &stderr2))
	require.Empty(t, stderr2.String())
	require.Contains(t, stdout2.String(), `"reverted"`)
}

func TestRunbookUpToValidation(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)
	_, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)

	t.Run("to 等于当前版 = no-op 退 0（CI 幂等，D14）", func(t *testing.T) {
		summary, err := runUpOnWorld(t, w, dir, func(o *RunOptions) { o.To, o.ToSet = 2, true })
		require.NoError(t, err)
		require.Empty(t, summary.Applied)
	})

	t.Run("to 低于当前版 = UsageError", func(t *testing.T) {
		_, err := runUpOnWorld(t, w, dir, func(o *RunOptions) { o.To, o.ToSet = 1, true })
		var ue *UsageError
		require.ErrorAs(t, err, &ue)
		require.Contains(t, err.Error(), "runbook down")
	})

	t.Run("to 超出本地最大版 = UsageError", func(t *testing.T) {
		_, err := runUpOnWorld(t, w, dir, func(o *RunOptions) { o.To, o.ToSet = 5, true })
		var ue *UsageError
		require.ErrorAs(t, err, &ue)
		require.Contains(t, err.Error(), "no local runbook file")
	})

	t.Run("to 中途版本只应用到该版", func(t *testing.T) {
		w2 := newFakeRunbookWorld()
		summary, err := runUpOnWorld(t, w2, dir, func(o *RunOptions) { o.To, o.ToSet = 1, true })
		require.NoError(t, err)
		require.Equal(t, int64(1), summary.CurrentVersion)
		w2.mu.Lock()
		defer w2.mu.Unlock()
		require.NotContains(t, w2.collections, w2.collectionKey("gold", "configs"))
	})
}

// ---------------------------------------------------------------------------
// down：默认单步 / --all / 不可逆 / 顶版撞收敛 / delete NotFound
// ---------------------------------------------------------------------------

func applyFixtureFully(t *testing.T, w *fakeRunbookWorld, dir string) {
	t.Helper()
	_, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
}

func TestRunbookDownDefaultSingleStep(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)
	applyFixtureFully(t, w, dir)

	summary, err := runDownOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.CurrentVersion)
	require.Len(t, summary.Reverted, 1)
	require.Equal(t, int64(2), summary.Reverted[0].Version)
	require.Equal(t, "delete_collection", summary.Reverted[0].Actions[0].Verb)
	require.Equal(t, "deleted", summary.Reverted[0].Actions[0].Result)

	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.steps, 1)
	require.NotContains(t, w.collections, w.collectionKey("gold", "configs"), "down 删除集合")
	require.Contains(t, w.databases, "gold", "默认只回退一步")
}

func TestRunbookDownAllThenUpCycle(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)
	applyFixtureFully(t, w, dir)

	summary, err := runDownOnWorld(t, w, dir, func(o *RunOptions) { o.All = true })
	require.NoError(t, err)
	require.Equal(t, int64(0), summary.CurrentVersion)
	require.Len(t, summary.Reverted, 2)
	w.mu.Lock()
	require.Empty(t, w.steps)
	require.Empty(t, w.collections)
	require.Empty(t, w.databases)
	w.mu.Unlock()

	// up → status → down → up 循环（任务验收点）：重新拉起。
	_, err = runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	statusOut := runStatusOnWorld(t, w, dir)
	require.True(t, statusOut.Clean)
	require.Equal(t, int64(2), statusOut.CurrentVersion)

	summary, err = runDownOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.CurrentVersion)
	_, err = runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
}

func TestRunbookDownIrreversible(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeRunbookDir(t, map[string]string{"000001_noop.yaml": "up: []\ndown: []\n"})
	_, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)

	_, err = runDownOnWorld(t, w, dir, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "irreversible")
	require.Contains(t, err.Error(), "000001")

	w.mu.Lock()
	require.Len(t, w.steps, 1, "不可逆版本不被摘除")
	w.mu.Unlock()

	// dry-run 预报不可逆，仍退 0。
	var out strings.Builder
	err = RunDown(w.caller, RunOptions{Dir: dir, DryRun: true}, &out, &strings.Builder{})
	require.NoError(t, err)
	var summary DownSummary
	require.NoError(t, decodeJSON(out.String(), &summary))
	require.Contains(t, summary.WouldFail, "irreversible")
}

func TestRunbookDownNothingApplied(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)
	summary, err := runDownOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Empty(t, summary.Reverted)
	require.Equal(t, int64(0), summary.CurrentVersion)
}

func TestRunbookDownTopCollisionConverges(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)
	applyFixtureFully(t, w, dir)

	// 并发方在引擎 Delete 前已摘掉顶版。
	w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
		if method == runbookMethodDelete && fakeReqInt(req, "version") == 2 {
			if len(w.steps) > 0 {
				w.steps = w.steps[:len(w.steps)-1]
			}
			return fakeRunbookErr(runbookCodeFailedPrecondition, "top moved")
		}
		return nil
	}
	summary, err := runDownOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.CurrentVersion)
	require.Len(t, summary.Reverted, 1, "本步动作由我们执行，计入 reverted")
	require.Equal(t, int64(2), summary.Reverted[0].Version)
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.steps, 1)
	require.NotContains(t, w.collections, w.collectionKey("gold", "configs"))
}

func TestRunbookDownDeleteNotFoundIsSuccess(t *testing.T) {
	// down 的 delete 动作撞 NotFound 一律按成功（§2.3）。
	w := newFakeRunbookWorld()
	dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})
	_, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)

	// 手工把库删了（资源已不在）。
	w.mu.Lock()
	delete(w.databases, "gold")
	w.mu.Unlock()

	summary, err := runDownOnWorld(t, w, dir, func(o *RunOptions) { o.All = true })
	require.NoError(t, err)
	require.Equal(t, "skipped", summary.Reverted[0].Actions[0].Result)
}

func TestRunbookDownToValidation(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEngineFixtureDir(t)
	applyFixtureFully(t, w, dir)

	t.Run("to 等于当前版 = UsageError", func(t *testing.T) {
		_, err := runDownOnWorld(t, w, dir, func(o *RunOptions) { o.To, o.ToSet = 2, true })
		var ue *UsageError
		require.ErrorAs(t, err, &ue)
	})

	t.Run("to 为负 = UsageError", func(t *testing.T) {
		_, err := runDownOnWorld(t, w, dir, func(o *RunOptions) { o.To, o.ToSet = -1, true })
		var ue *UsageError
		require.ErrorAs(t, err, &ue)
	})

	t.Run("to 与 all 同给 = UsageError", func(t *testing.T) {
		_, err := runDownOnWorld(t, w, dir, func(o *RunOptions) { o.To, o.ToSet, o.All = 1, true, true })
		var ue *UsageError
		require.ErrorAs(t, err, &ue)
	})

	t.Run("to 0 等价 all", func(t *testing.T) {
		summary, err := runDownOnWorld(t, w, dir, func(o *RunOptions) { o.To, o.ToSet = 0, true })
		require.NoError(t, err)
		require.Equal(t, int64(0), summary.CurrentVersion)
	})
}

// ---------------------------------------------------------------------------
// status（D16）
// ---------------------------------------------------------------------------

func runStatusOnWorld(t *testing.T, w *fakeRunbookWorld, dir string) StatusOut {
	t.Helper()
	var out strings.Builder
	require.NoError(t, RunStatus(w.caller, dir, &out))
	var res StatusOut
	require.NoError(t, decodeJSON(out.String(), &res))
	return res
}

func TestRunbookStatusStates(t *testing.T) {
	t.Run("全部 applied 且 clean", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeEngineFixtureDir(t)
		applyFixtureFully(t, w, dir)
		res := runStatusOnWorld(t, w, dir)
		require.True(t, res.Clean)
		require.Equal(t, int64(2), res.CurrentVersion)
		require.Len(t, res.Files, 2)
		require.Equal(t, "applied", res.Files[0].State)
		require.Equal(t, "applied", res.Files[1].State)
		require.False(t, res.Files[0].Irreversible)
	})

	t.Run("pending：未应用", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeEngineFixtureDir(t)
		res := runStatusOnWorld(t, w, dir)
		require.True(t, res.Clean, "pending 不是脏状态")
		require.Equal(t, int64(0), res.CurrentVersion)
		require.Equal(t, "pending", res.Files[0].State)
	})

	t.Run("modified：checksum 不符", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})
		_, err := runUpOnWorld(t, w, dir, nil)
		require.NoError(t, err)

		tampered := strings.Replace(runbookFixtureCreateDB, "Gold Store", "Drifted", 1)
		dir2 := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": tampered})
		res := runStatusOnWorld(t, w, dir2)
		require.False(t, res.Clean)
		require.Equal(t, "modified", res.Files[0].State)
	})

	t.Run("orphan：服务端有本地无", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeEngineFixtureDir(t)
		applyFixtureFully(t, w, dir)
		dir2 := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})
		res := runStatusOnWorld(t, w, dir2)
		require.False(t, res.Clean)
		require.Len(t, res.Files, 2)
		require.Equal(t, "applied", res.Files[0].State)
		require.Equal(t, "orphan", res.Files[1].State)
		require.Equal(t, "create_configs_coll", res.Files[1].Name)
	})

	t.Run("irreversible 标记空 down 段", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeRunbookDir(t, map[string]string{"000001_noop.yaml": "up: []\n"})
		res := runStatusOnWorld(t, w, dir)
		require.True(t, res.Files[0].Irreversible)
	})
}

// ---------------------------------------------------------------------------
// forgive（D21）
// ---------------------------------------------------------------------------

func TestRunbookForgive(t *testing.T) {
	t.Run("从顶版逐个摘到 N，不动资源", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeEngineFixtureDir(t)
		applyFixtureFully(t, w, dir)
		require.True(t, w.hasDDLCall())

		w.mu.Lock()
		w.calls = nil // 清掉 up 期间的调用记录
		w.mu.Unlock()

		var out strings.Builder
		require.NoError(t, RunForgive(w.caller, dir, 2, &out))
		var res ForgiveSummary
		require.NoError(t, decodeJSON(out.String(), &res))
		require.Equal(t, []int64{2}, res.DeletedVersions)
		require.Equal(t, int64(1), res.CurrentVersion)

		// 摘三步到 1。
		var out2 strings.Builder
		require.NoError(t, RunForgive(w.caller, dir, 1, &out2))
		var res2 ForgiveSummary
		require.NoError(t, decodeJSON(out2.String(), &res2))
		require.Equal(t, []int64{1}, res2.DeletedVersions)
		require.Equal(t, int64(0), res2.CurrentVersion)

		require.False(t, w.hasDDLCall(), "forgive 不执行任何资源动作（D21）")
		w.mu.Lock()
		defer w.mu.Unlock()
		require.Empty(t, w.steps)
		require.Contains(t, w.databases, "gold", "资源保持原样")
		require.Contains(t, w.collections, w.collectionKey("gold", "configs"))
	})

	t.Run("未应用版本不可 forgive", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeEngineFixtureDir(t)
		err := RunForgive(w.caller, dir, 2, &strings.Builder{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not applied")
	})

	t.Run("本地无该版本文件不可 forgive", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeEngineFixtureDir(t)
		applyFixtureFully(t, w, dir)
		err := RunForgive(w.caller, dir, 5, &strings.Builder{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "no local runbook file")
	})

	t.Run("forgive 后重跑 up 以新 checksum 重放（注释级改动不触资源）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})
		_, err := runUpOnWorld(t, w, dir, nil)
		require.NoError(t, err)

		// 只加注释：checksum 变、声明不变 → up 先被拦，forgive 后以 skip 重放。
		tampered := "# edited comment\n" + runbookFixtureCreateDB
		dir2 := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": tampered})
		_, err = runUpOnWorld(t, w, dir2, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "checksum mismatch")

		require.NoError(t, RunForgive(w.caller, dir2, 1, &strings.Builder{}))
		summary, err := runUpOnWorld(t, w, dir2, nil)
		require.NoError(t, err)
		require.Equal(t, int64(1), summary.CurrentVersion)
		require.Len(t, summary.Applied, 1)
		require.Equal(t, "skipped", summary.Applied[0].Actions[0].Result, "动作幂等 skip + Record 新 checksum")
	})
}

// TestRunbookForgiveThenUpWithDriftLocked 钉死语义：forgive 只摘记录，若文件
// 改动引入配置漂移（线上资源仍是旧形状），重跑 up 必须按 D6 fail 而非静默改。
func TestRunbookForgiveThenUpWithDriftLocked(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": runbookFixtureCreateDB})
	_, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)

	tampered := strings.Replace(runbookFixtureCreateDB, "Gold Store", "Renamed Store", 1)
	dir2 := writeRunbookDir(t, map[string]string{"000001_create_gold_db.yaml": tampered})
	_, err = runUpOnWorld(t, w, dir2, nil)
	require.Error(t, err)

	require.NoError(t, RunForgive(w.caller, dir2, 1, &strings.Builder{}))
	_, err = runUpOnWorld(t, w, dir2, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "name")
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Empty(t, w.steps, "drift 失败不记录版本")
	require.Equal(t, "Gold Store", w.databases["gold"]["name"], "线上资源未被悄悄改动")
}

// decodeJSON 以 UseNumber 解码（CLI 包的 decodeJSON 副本：summary 的 int64
// 字段断言不吃 float64 精度亏）。
func decodeJSON(s string, v any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	return dec.Decode(v)
}
