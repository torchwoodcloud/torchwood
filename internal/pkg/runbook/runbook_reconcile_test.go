package runbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 动词 schema 校验（任务 8：字段级、静态表驱动）
// ---------------------------------------------------------------------------

func TestValidateRunbookVerbs(t *testing.T) {
	load := func(content string) []runbookFile {
		t.Helper()
		dir := writeRunbookDir(t, map[string]string{"000001_x.yaml": content})
		files, err := loadRunbookDir(dir)
		require.NoError(t, err)
		return files
	}

	t.Run("合法动作过", func(t *testing.T) {
		files := load(`up:
  - create_database: { id: gold, name: Gold }
  - create_collection:
      database_id: gold
      id: configs
      name: configs
      permissions: ['read:users']
      document_security: true
      attributes:
        - { key: key, type: string, size: 64, required: true }
        - { key: emb, type: json, default_value: '{}', array: true }
      indexes:
        - { id: key, type: unique, attributes: [key], orders: [ASC] }
down:
  - delete_collection: { database_id: gold, collection_id: configs }
  - delete_database: { id: gold }
`)
		require.NoError(t, validateRunbookVerbs(files))
	})

	t.Run("全部十动词过", func(t *testing.T) {
		files := load(`up:
  - create_database: { id: a, name: A }
  - update_collection: { database_id: a, collection_id: c, name: C, permissions: { values: ['read:users'] }, disabled: true }
  - create_attribute: { database_id: a, collection_id: c, key: k, type: string, size: 8, required: true, array: false, default_value: x, dims: 0 }
  - delete_attribute: { database_id: a, collection_id: c, key: k }
  - restore_attribute: { database_id: a, collection_id: c, key: k }
  - create_index: { database_id: a, collection_id: c, id: i, type: key, attributes: [k], orders: [ASC], distance_metric: COSINE }
  - delete_index: { database_id: a, collection_id: c, index_id: i }
  - delete_collection: { database_id: a, collection_id: c }
down:
  - delete_database: { id: a }
`)
		require.NoError(t, validateRunbookVerbs(files))
	})

	t.Run("未知动词报错并列出词表", func(t *testing.T) {
		files := load("up:\n  - create_universe: {}\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown verb")
		require.Contains(t, err.Error(), "create_universe")
		require.Contains(t, err.Error(), "create_collection")
	})

	t.Run("旧推定动词名（create_board 等）报 unknown verb（阶段 D 统一资源全名）", func(t *testing.T) {
		// 阶段 D 动词已落地并统一为资源全名（create_leaderboard_board 等），
		// C 阶段的推定名 create_board/update_board 回归 unknown verb。
		for _, verb := range []string{"create_board", "update_board"} {
			files := load("up:\n  - " + verb + ": {}\n")
			err := validateRunbookVerbs(files)
			require.Error(t, err, verb)
			require.Contains(t, err.Error(), "unknown verb", verb)
			require.Contains(t, err.Error(), verb)
		}
	})

	t.Run("create_collection 用 collection_id 报未知字段（字段面=proto 字段集）", func(t *testing.T) {
		files := load("up:\n  - create_collection:\n      database_id: gold\n      collection_id: configs\n      name: configs\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), `unknown field "collection_id"`)
		require.Contains(t, err.Error(), "id")
	})

	t.Run("缺必填引用键", func(t *testing.T) {
		files := load("up:\n  - create_collection:\n      database_id: gold\n      name: configs\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), `missing required field "id"`)
	})

	t.Run("必填字符串不得为空", func(t *testing.T) {
		files := load("up:\n  - create_database: { id: '', name: x }\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must not be empty")
	})

	t.Run("类型不符：size 非整数", func(t *testing.T) {
		files := load("up:\n  - create_attribute: { database_id: a, collection_id: c, key: k, type: string, size: big }\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be an integer")
	})

	t.Run("类型不符：document_security 非布尔", func(t *testing.T) {
		files := load("up:\n  - create_collection: { database_id: a, id: c, name: c, document_security: 'yes' }\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be a boolean")
	})

	t.Run("类型不符：permissions 非字符串列表", func(t *testing.T) {
		files := load("up:\n  - create_collection: { database_id: a, id: c, name: c, permissions: [1, 2] }\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be a string")
	})

	t.Run("update_collection 的 permissions 须是 PermissionsUpdate 形状", func(t *testing.T) {
		files := load("up:\n  - update_collection: { database_id: a, collection_id: c, permissions: ['read:users'] }\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), "PermissionsUpdate")
	})

	t.Run("内嵌属性缺 key", func(t *testing.T) {
		files := load("up:\n  - create_collection:\n      database_id: a\n      id: c\n      name: c\n      attributes:\n        - { type: string }\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), `missing required field "key"`)
	})

	t.Run("内嵌索引未知字段", func(t *testing.T) {
		files := load("up:\n  - create_collection:\n      database_id: a\n      id: c\n      name: c\n      indexes:\n        - { id: i, type: key, columns: [k] }\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), `unknown field "columns"`)
	})

	t.Run("down 段同样校验", func(t *testing.T) {
		files := load("up: []\ndown:\n  - delete_collection: { database_id: a }\n")
		err := validateRunbookVerbs(files)
		require.Error(t, err)
		require.Contains(t, err.Error(), "down action #1")
		require.Contains(t, err.Error(), `missing required field "collection_id"`)
	})

	t.Run("引擎目录加载串联 schema 校验", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{"000001_x.yaml": "up:\n  - create_universe: {}\n"})
		_, err := loadRunbookEngineDir(dir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown verb")
	})
}

// ---------------------------------------------------------------------------
// D12/D20 比较白名单 fixture（缺省值两侧等价）
// ---------------------------------------------------------------------------

func TestCompareRunbookDatabase(t *testing.T) {
	t.Run("name 相等（读回的元数据字段不参与）", func(t *testing.T) {
		got := map[string]any{"id": "gold", "name": "Gold Store", "createdAt": "2026-09-01T00:00:00Z", "updatedAt": "2026-09-02T00:00:00Z"}
		want := map[string]any{"id": "gold", "name": "Gold Store"}
		require.Empty(t, compareRunbookDatabase(want, got))
	})

	t.Run("name 不等产出结构化 diff", func(t *testing.T) {
		diffs := compareRunbookDatabase(map[string]any{"name": "Gold"}, map[string]any{"name": "Silver"})
		require.Len(t, diffs, 1)
		require.Equal(t, "name", diffs[0].Field)
		require.Equal(t, "Gold", diffs[0].Want)
		require.Equal(t, "Silver", diffs[0].Got)
	})
}

func TestCompareRunbookCollection(t *testing.T) {
	t.Run("permissions 顺序无关（D20）", func(t *testing.T) {
		want := map[string]any{"name": "c", "permissions": []any{"read:users", "read:keys"}}
		got := map[string]any{"name": "c", "permissions": []any{"read:keys", "read:users"}}
		require.Empty(t, compareRunbookCollection(want, got))
	})

	t.Run("write 声明与读回展开等价（服务端写侧展开的镜像归一）", func(t *testing.T) {
		want := map[string]any{"name": "c", "permissions": []any{"write:keys"}}
		got := map[string]any{"name": "c", "permissions": []any{"update:keys", "create:keys", "delete:keys"}}
		require.Empty(t, compareRunbookCollection(want, got))
	})

	t.Run("声明未写 permissions 不比较（服务端赋缺省集）", func(t *testing.T) {
		want := map[string]any{"name": "c"}
		got := map[string]any{"name": "c", "permissions": []any{"create:users", "read:keys"}}
		require.Empty(t, compareRunbookCollection(want, got))
	})

	t.Run("声明 permissions 与读回不等出 diff", func(t *testing.T) {
		want := map[string]any{"name": "c", "permissions": []any{"read:users"}}
		got := map[string]any{"name": "c", "permissions": []any{"read:keys"}}
		diffs := compareRunbookCollection(want, got)
		require.Len(t, diffs, 1)
		require.Equal(t, "permissions", diffs[0].Field)
	})

	t.Run("document_security 缺省 false 两侧等价", func(t *testing.T) {
		want := map[string]any{"name": "c"} // 未声明
		got := map[string]any{"name": "c"}  // protojson 省略 false
		require.Empty(t, compareRunbookCollection(want, got))
	})

	t.Run("create 侧期望 disabled=false，线上 disabled 出 diff", func(t *testing.T) {
		want := map[string]any{"name": "c"}
		got := map[string]any{"name": "c", "disabled": true}
		diffs := compareRunbookCollection(want, got)
		require.Len(t, diffs, 1)
		require.Equal(t, "disabled", diffs[0].Field)
	})
}

func TestCompareRunbookAttribute(t *testing.T) {
	t.Run("size 整型跨形态等价（yaml int vs json.Number）", func(t *testing.T) {
		want := map[string]any{"type": "string", "size": 64, "required": true}
		got := map[string]any{"type": "string", "size": json.Number("64"), "required": true}
		require.Empty(t, compareRunbookAttribute(want, got))
	})

	t.Run("缺省值两侧等价：size/required/array/default_value/dims 全零", func(t *testing.T) {
		want := map[string]any{"type": "json"}
		got := map[string]any{"type": "json"}
		require.Empty(t, compareRunbookAttribute(want, got))
	})

	t.Run("dims 显式声明与缺省不等", func(t *testing.T) {
		want := map[string]any{"type": "vector", "dims": 3}
		got := map[string]any{"type": "vector"}
		diffs := compareRunbookAttribute(want, got)
		require.Len(t, diffs, 1)
		require.Equal(t, "dims", diffs[0].Field)
	})

	t.Run("required 差异出 diff", func(t *testing.T) {
		want := map[string]any{"type": "string", "required": true}
		got := map[string]any{"type": "string"}
		diffs := compareRunbookAttribute(want, got)
		require.Len(t, diffs, 1)
		require.Equal(t, "required", diffs[0].Field)
		require.Equal(t, true, diffs[0].Want)
		require.Equal(t, false, diffs[0].Got)
	})

	t.Run("status 不在白名单", func(t *testing.T) {
		want := map[string]any{"type": "string"}
		got := map[string]any{"type": "string", "status": "deprecated"}
		require.Empty(t, compareRunbookAttribute(want, got))
	})
}

func TestCompareRunbookIndex(t *testing.T) {
	t.Run("相等", func(t *testing.T) {
		want := map[string]any{"type": "unique", "attributes": []any{"a", "b"}, "orders": []any{"ASC", "DESC"}}
		got := map[string]any{"type": "unique", "attributes": []any{"a", "b"}, "orders": []any{"ASC", "DESC"}}
		require.Empty(t, compareRunbookIndex(want, got))
	})

	t.Run("attributes 顺序敏感（索引列序有语义）", func(t *testing.T) {
		want := map[string]any{"type": "key", "attributes": []any{"a", "b"}}
		got := map[string]any{"type": "key", "attributes": []any{"b", "a"}}
		diffs := compareRunbookIndex(want, got)
		require.Len(t, diffs, 1)
		require.Equal(t, "attributes", diffs[0].Field)
	})

	t.Run("hnsw 缺省 metric 与读回 COSINE 等价（服务端归一镜像）", func(t *testing.T) {
		want := map[string]any{"type": "hnsw", "attributes": []any{"v"}}
		got := map[string]any{"type": "hnsw", "attributes": []any{"v"}, "distanceMetric": "COSINE"}
		require.Empty(t, compareRunbookIndex(want, got))
	})

	t.Run("hnsw metric 小写声明与读回大写等价", func(t *testing.T) {
		want := map[string]any{"type": "hnsw", "attributes": []any{"v"}, "distance_metric": "cosine"}
		got := map[string]any{"type": "hnsw", "attributes": []any{"v"}, "distanceMetric": "COSINE"}
		require.Empty(t, compareRunbookIndex(want, got))
	})

	t.Run("非 hnsw 不比较 metric（读回恒空）", func(t *testing.T) {
		want := map[string]any{"type": "key", "attributes": []any{"a"}}
		got := map[string]any{"type": "key", "attributes": []any{"a"}}
		require.Empty(t, compareRunbookIndex(want, got))
	})

	t.Run("metric 不等出 diff", func(t *testing.T) {
		want := map[string]any{"type": "hnsw", "attributes": []any{"v"}, "distance_metric": "L2"}
		got := map[string]any{"type": "hnsw", "attributes": []any{"v"}, "distanceMetric": "COSINE"}
		diffs := compareRunbookIndex(want, got)
		require.Len(t, diffs, 1)
		require.Equal(t, "distance_metric", diffs[0].Field)
	})
}

func TestRunbookDriftErrorRendering(t *testing.T) {
	err := &runbookDriftError{
		resource: "database gold",
		diffs: []runbookFieldDiff{
			{Field: "name", Want: "Gold Store", Got: "Tampered"},
			{Field: "permissions", Want: []string{"read:users"}, Got: []string{"read:keys"}},
		},
	}
	msg := err.Error()
	require.Contains(t, msg, "database gold")
	require.Contains(t, msg, "name: want \"Gold Store\", got \"Tampered\"")
	require.Contains(t, msg, "permissions: want [read:users], got [read:keys]")
	require.Contains(t, msg, "NEW runbook step")
}

// ---------------------------------------------------------------------------
// 决策层：reconcile 四分支 + attribute 生命周期 + TOCTOU 收敛
// ---------------------------------------------------------------------------

func runReconcileAction(t *testing.T, w *fakeRunbookWorld, verb string, body map[string]any) (runbookActionOutcome, error) {
	t.Helper()
	rec := &runbookReconciler{caller: w.caller}
	return rec.action(runbookAction{Verb: verb, Body: body})
}

func TestReconcileCreateDatabaseBranches(t *testing.T) {
	t.Run("create 分支：不存在 → 建", func(t *testing.T) {
		w := newFakeRunbookWorld()
		out, err := runReconcileAction(t, w, "create_database", map[string]any{"id": "gold", "name": "Gold"})
		require.NoError(t, err)
		require.Equal(t, "created", out.Report.Result)
		require.Equal(t, "gold", out.Report.Target)
		w.mu.Lock()
		defer w.mu.Unlock()
		require.Contains(t, w.databases, "gold")
	})

	t.Run("skip 分支：存在且相等", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.mu.Unlock()
		out, err := runReconcileAction(t, w, "create_database", map[string]any{"id": "gold", "name": "Gold"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("fail 分支：存在但不等（带 diff）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Other"}
		w.mu.Unlock()
		_, err := runReconcileAction(t, w, "create_database", map[string]any{"id": "gold", "name": "Gold"})
		require.Error(t, err)
		var drift *runbookDriftError
		require.ErrorAs(t, err, &drift)
	})

	t.Run("TOCTOU：create 撞 AlreadyExists 后重读相等 → skip", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookDBMethod("CreateDatabase") {
				w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"} // 并发方建了同形资源
				return fakeRunbookErr(runbookCodeAlreadyExists, "database already exists")
			}
			return nil
		}
		out, err := runReconcileAction(t, w, "create_database", map[string]any{"id": "gold", "name": "Gold"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("TOCTOU：撞后重读不等 → fail", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookDBMethod("CreateDatabase") {
				w.databases["gold"] = map[string]any{"id": "gold", "name": "Different"}
				return fakeRunbookErr(runbookCodeAlreadyExists, "database already exists")
			}
			return nil
		}
		_, err := runReconcileAction(t, w, "create_database", map[string]any{"id": "gold", "name": "Gold"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "Different")
	})
}

func TestReconcileCreateCollectionFullSet(t *testing.T) {
	decl := func() map[string]any {
		return map[string]any{
			"database_id": "gold", "id": "configs", "name": "configs",
			"document_security": true,
			"permissions":       []any{"read:users"},
			"attributes": []any{
				map[string]any{"key": "key", "type": "string", "size": 64, "required": true},
				map[string]any{"key": "value", "type": "json"},
			},
			"indexes": []any{
				map[string]any{"id": "key", "type": "unique", "attributes": []any{"key"}},
			},
		}
	}
	seededDB := func(w *fakeRunbookWorld) {
		w.mu.Lock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.mu.Unlock()
	}

	t.Run("补建分支：集合存在、属性缺一半 → 只补缺", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seededDB(w)
		w.seedCollection("gold", "configs", "configs", []any{"read:users"}, true,
			[]any{map[string]any{"key": "key", "type": "string", "size": int64(64), "required": true}}, // value 缺
			[]any{}) // key 索引缺
		out, err := runReconcileAction(t, w, "create_collection", decl())
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result, "集合本身相等 skip")
		// 进度事件按子操作展开。
		results := map[string]string{}
		for _, ev := range out.Events {
			results[ev.Verb+"/"+ev.Target] = ev.Result
		}
		require.Equal(t, "created", results["create_attribute/gold.configs.value"])
		require.Equal(t, "created", results["create_index/gold.configs.key"])
		coll := w.fakeFindAttribute("gold", "configs", "value")
		require.NotNil(t, coll, "缺的属性被补建")
	})

	t.Run("多出 fail：线上有未声明属性", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seededDB(w)
		w.seedCollection("gold", "configs", "configs", []any{"read:users"}, true,
			[]any{
				map[string]any{"key": "key", "type": "string", "size": int64(64), "required": true},
				map[string]any{"key": "value", "type": "json"},
				map[string]any{"key": "rogue", "type": "string"}, // 未声明
			},
			[]any{map[string]any{"id": "key", "type": "unique", "attributes": []any{"key"}}})
		_, err := runReconcileAction(t, w, "create_collection", decl())
		require.Error(t, err)
		require.Contains(t, err.Error(), "rogue")
		require.Contains(t, err.Error(), "full set")
	})

	t.Run("多出 fail：线上有未声明索引", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seededDB(w)
		w.seedCollection("gold", "configs", "configs", []any{"read:users"}, true,
			[]any{
				map[string]any{"key": "key", "type": "string", "size": int64(64), "required": true},
				map[string]any{"key": "value", "type": "json"},
			},
			[]any{
				map[string]any{"id": "key", "type": "unique", "attributes": []any{"key"}},
				map[string]any{"id": "rogue", "type": "key", "attributes": []any{"value"}},
			})
		_, err := runReconcileAction(t, w, "create_collection", decl())
		require.Error(t, err)
		require.Contains(t, err.Error(), "rogue")
	})

	t.Run("retired 未声明属性不触发多出 fail（物理列已删）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seededDB(w)
		w.seedCollection("gold", "configs", "configs", []any{"read:users"}, true,
			[]any{
				map[string]any{"key": "key", "type": "string", "size": int64(64), "required": true},
				map[string]any{"key": "value", "type": "json"},
				map[string]any{"key": "gone", "type": "string", "status": "retired"},
			},
			[]any{map[string]any{"id": "key", "type": "unique", "attributes": []any{"key"}}})
		_, err := runReconcileAction(t, w, "create_collection", decl())
		require.NoError(t, err)
	})

	t.Run("全新创建：集合+属性+索引逐个落地", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seededDB(w)
		out, err := runReconcileAction(t, w, "create_collection", decl())
		require.NoError(t, err)
		require.Equal(t, "created", out.Report.Result)
		require.Len(t, out.Events, 4, "集合 + 2 属性 + 1 索引")
		require.NotNil(t, w.fakeFindAttribute("gold", "configs", "key"))
		require.NotNil(t, w.fakeFindAttribute("gold", "configs", "value"))
	})

	t.Run("dry-run：只读不写，子操作全 planned", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seededDB(w)
		rec := &runbookReconciler{caller: w.caller, dryRun: true}
		out, err := rec.action(runbookAction{Verb: "create_collection", Body: decl()})
		require.NoError(t, err)
		require.Equal(t, "planned", out.Report.Result)
		for _, ev := range out.Events {
			require.Equal(t, "planned", ev.Result)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		require.NotContains(t, w.collections, w.collectionKey("gold", "configs"))
	})
}

func TestReconcileAttributeLifecycle(t *testing.T) {
	seedCollWithAttr := func(w *fakeRunbookWorld, attr map[string]any) {
		w.mu.Lock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.collections[w.collectionKey("gold", "configs")] = map[string]any{
			"id": "configs", "databaseId": "gold", "name": "configs",
			"documentSecurity": true,
			"attributes":       []any{attr},
			"indexes":          []any{},
		}
		w.mu.Unlock()
	}
	wantAttr := func() map[string]any {
		return map[string]any{
			"database_id": "gold", "collection_id": "configs",
			"key": "k", "type": "string", "size": 64, "required": true,
		}
	}
	onlineAttr := func(extra map[string]any) map[string]any {
		m := map[string]any{"key": "k", "type": "string", "size": int64(64), "required": true}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	t.Run("active 相等 → skip", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seedCollWithAttr(w, onlineAttr(nil))
		out, err := runReconcileAction(t, w, "create_attribute", wantAttr())
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("active 不等 → fail 带 diff", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seedCollWithAttr(w, onlineAttr(map[string]any{"size": int64(128)}))
		_, err := runReconcileAction(t, w, "create_attribute", wantAttr())
		require.Error(t, err)
		require.Contains(t, err.Error(), "size")
	})

	t.Run("deprecated 相等 → Restore 复活", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seedCollWithAttr(w, onlineAttr(map[string]any{"status": "deprecated"}))
		out, err := runReconcileAction(t, w, "create_attribute", wantAttr())
		require.NoError(t, err)
		require.Equal(t, "restored", out.Report.Result)
		attr := w.fakeFindAttribute("gold", "configs", "k")
		require.NotContains(t, attr, "status", "回到 active（省略）")
	})

	t.Run("deprecated 不等 → fail（复活前先比较）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seedCollWithAttr(w, onlineAttr(map[string]any{"status": "deprecated", "size": int64(999)}))
		_, err := runReconcileAction(t, w, "create_attribute", wantAttr())
		require.Error(t, err)
		require.Contains(t, err.Error(), "size")
		attr := w.fakeFindAttribute("gold", "configs", "k")
		require.Equal(t, "deprecated", attr["status"], "不等时不复活")
	})

	t.Run("retired → 视为不存在正常建", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seedCollWithAttr(w, onlineAttr(map[string]any{"status": "retired"}))
		out, err := runReconcileAction(t, w, "create_attribute", wantAttr())
		require.NoError(t, err)
		require.Equal(t, "created", out.Report.Result)
		attr := w.fakeFindAttribute("gold", "configs", "k")
		require.NotContains(t, attr, "status")
	})

	t.Run("migrating → fail 提示重跑", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seedCollWithAttr(w, onlineAttr(map[string]any{"status": "migrating"}))
		_, err := runReconcileAction(t, w, "create_attribute", wantAttr())
		require.Error(t, err)
		require.Contains(t, err.Error(), "migrating")
		require.Contains(t, err.Error(), "re-run")
	})

	t.Run("TOCTOU：CreateAttribute 撞 AlreadyExists 后重读相等 → skip", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.collections[w.collectionKey("gold", "configs")] = map[string]any{
			"id": "configs", "databaseId": "gold", "name": "configs",
			"attributes": []any{}, "indexes": []any{},
		}
		w.mu.Unlock()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookDBMethod("CreateAttribute") {
				w.collections[w.collectionKey("gold", "configs")]["attributes"] = []any{
					map[string]any{"key": "k", "type": "string", "size": int64(64), "required": true},
				}
				return fakeRunbookErr(runbookCodeAlreadyExists, "attribute already exists")
			}
			return nil
		}
		out, err := runReconcileAction(t, w, "create_attribute", wantAttr())
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("集合不存在 → 硬错误", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_attribute", wantAttr())
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not exist")
	})
}

func TestReconcileDeleteVerbs(t *testing.T) {
	seed := func(w *fakeRunbookWorld) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.collections[w.collectionKey("gold", "configs")] = map[string]any{
			"id": "configs", "databaseId": "gold", "name": "configs",
			"attributes": []any{map[string]any{"key": "k", "type": "string", "required": true}},
			"indexes":    []any{map[string]any{"id": "key", "type": "unique", "attributes": []any{"k"}}},
		}
	}

	t.Run("delete_database 存在 → deleted", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seed(w)
		out, err := runReconcileAction(t, w, "delete_database", map[string]any{"id": "gold"})
		require.NoError(t, err)
		require.Equal(t, "deleted", out.Report.Result)
		w.mu.Lock()
		defer w.mu.Unlock()
		require.NotContains(t, w.databases, "gold")
	})

	t.Run("delete_database 不存在 → skipped（NotFound 一律按成功）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		out, err := runReconcileAction(t, w, "delete_database", map[string]any{"id": "nope"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("delete_attribute active → 软删 deprecated", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seed(w)
		out, err := runReconcileAction(t, w, "delete_attribute", map[string]any{"database_id": "gold", "collection_id": "configs", "key": "k"})
		require.NoError(t, err)
		require.Equal(t, "deleted", out.Report.Result)
		require.Equal(t, "deprecated", w.fakeFindAttribute("gold", "configs", "k")["status"])
	})

	t.Run("delete_attribute 已 deprecated → skipped（已是目标态）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seed(w)
		w.fakeFindAttribute("gold", "configs", "k")["status"] = "deprecated"
		out, err := runReconcileAction(t, w, "delete_attribute", map[string]any{"database_id": "gold", "collection_id": "configs", "key": "k"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("delete_attribute 集合整体不在 → skipped", func(t *testing.T) {
		w := newFakeRunbookWorld()
		out, err := runReconcileAction(t, w, "delete_attribute", map[string]any{"database_id": "gold", "collection_id": "configs", "key": "k"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("restore_attribute active → skipped；deprecated → restored；retired → 明确错误", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seed(w)
		out, err := runReconcileAction(t, w, "restore_attribute", map[string]any{"database_id": "gold", "collection_id": "configs", "key": "k"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)

		w.fakeFindAttribute("gold", "configs", "k")["status"] = "deprecated"
		out, err = runReconcileAction(t, w, "restore_attribute", map[string]any{"database_id": "gold", "collection_id": "configs", "key": "k"})
		require.NoError(t, err)
		require.Equal(t, "restored", out.Report.Result)

		w.fakeFindAttribute("gold", "configs", "k")["status"] = "retired"
		_, err = runReconcileAction(t, w, "restore_attribute", map[string]any{"database_id": "gold", "collection_id": "configs", "key": "k"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "retired")
	})

	t.Run("delete_index 存在 → deleted；不存在 → skipped", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seed(w)
		out, err := runReconcileAction(t, w, "delete_index", map[string]any{"database_id": "gold", "collection_id": "configs", "index_id": "key"})
		require.NoError(t, err)
		require.Equal(t, "deleted", out.Report.Result)

		out, err = runReconcileAction(t, w, "delete_index", map[string]any{"database_id": "gold", "collection_id": "configs", "index_id": "key"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("delete_collection 存在 → deleted；级联后属性动作 skipped", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seed(w)
		out, err := runReconcileAction(t, w, "delete_collection", map[string]any{"database_id": "gold", "collection_id": "configs"})
		require.NoError(t, err)
		require.Equal(t, "deleted", out.Report.Result)
		// 集合已删，后续同集合的 delete_attribute 自然 skip。
		out, err = runReconcileAction(t, w, "delete_attribute", map[string]any{"database_id": "gold", "collection_id": "configs", "key": "k"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})
}

func TestReconcileUpdateCollection(t *testing.T) {
	t.Run("直接执行不做比较（幂等），体直传", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.collections[w.collectionKey("gold", "configs")] = map[string]any{
			"id": "configs", "databaseId": "gold", "name": "configs",
			"permissions": []any{}, "attributes": []any{}, "indexes": []any{},
		}
		w.mu.Unlock()
		body := map[string]any{
			"database_id": "gold", "collection_id": "configs",
			"name": "renamed", "disabled": true,
		}
		out, err := runReconcileAction(t, w, "update_collection", body)
		require.NoError(t, err)
		require.Equal(t, "updated", out.Report.Result)
		w.mu.Lock()
		defer w.mu.Unlock()
		coll := w.collections[w.collectionKey("gold", "configs")]
		require.Equal(t, "renamed", coll["name"])
		require.Equal(t, true, coll["disabled"])
	})

	t.Run("目标不存在 → NotFound 上抛 fail", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "update_collection", map[string]any{"database_id": "gold", "collection_id": "nope", "name": "x"})
		require.Error(t, err)
		require.True(t, isRunbookCode(err, runbookCodeNotFound), "错误保留 RPC 分类与退出码映射")
	})
}

func TestReconcileTOCTOUCollection(t *testing.T) {
	// 集合级 TOCTOU：create 撞 AlreadyExists → 重读相等 skip / 不等 fail。
	decl := map[string]any{
		"database_id": "gold", "id": "configs", "name": "configs",
		"permissions": []any{"read:users"},
		"attributes":  []any{map[string]any{"key": "k", "type": "string"}},
	}

	t.Run("撞后相等 → skip + 子声明继续 reconcile", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.mu.Unlock()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookDBMethod("CreateCollection") {
				// intercept 已持锁：直接置内部字段（并发方建了同形集合）。
				w.collections[w.collectionKey("gold", "configs")] = map[string]any{
					"id": "configs", "databaseId": "gold", "name": "configs",
					"permissions": fakeExpandPermissions([]any{"read:users"}),
					"attributes":  []any{}, "indexes": []any{},
				}
				return fakeRunbookErr(runbookCodeAlreadyExists, "collection already exists")
			}
			return nil
		}
		out, err := runReconcileAction(t, w, "create_collection", decl)
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
		// 并发方没建属性 → 我们补建。
		require.NotNil(t, w.fakeFindAttribute("gold", "configs", "k"))
	})

	t.Run("撞后不等 → fail", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.mu.Unlock()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookDBMethod("CreateCollection") {
				w.collections[w.collectionKey("gold", "configs")] = map[string]any{
					"id": "configs", "databaseId": "gold", "name": "别人建的",
					"permissions": fakeExpandPermissions([]any{"read:users"}),
					"attributes":  []any{}, "indexes": []any{},
				}
				return fakeRunbookErr(runbookCodeAlreadyExists, "collection already exists")
			}
			return nil
		}
		_, err := runReconcileAction(t, w, "create_collection", decl)
		require.Error(t, err)
		require.Contains(t, err.Error(), "name")
	})
}

func TestReconcileIndexLifecycle(t *testing.T) {
	seed := func(w *fakeRunbookWorld, idxs ...map[string]any) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.databases["gold"] = map[string]any{"id": "gold", "name": "Gold"}
		w.collections[w.collectionKey("gold", "configs")] = map[string]any{
			"id": "configs", "databaseId": "gold", "name": "configs",
			"attributes": []any{map[string]any{"key": "k", "type": "string"}},
			"indexes":    toAnySlice(idxs),
		}
	}
	decl := map[string]any{
		"database_id": "gold", "collection_id": "configs",
		"id": "key", "type": "unique", "attributes": []any{"k"},
	}

	t.Run("存在相等 → skip；不等 → fail", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seed(w, map[string]any{"id": "key", "type": "unique", "attributes": []any{"k"}})
		out, err := runReconcileAction(t, w, "create_index", decl)
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)

		w2 := newFakeRunbookWorld()
		seed(w2, map[string]any{"id": "key", "type": "key", "attributes": []any{"k"}})
		_, err = runReconcileAction(t, w2, "create_index", decl)
		require.Error(t, err)
		require.Contains(t, err.Error(), "type")
	})

	t.Run("TOCTOU：撞 AlreadyExists 后重读相等 → skip", func(t *testing.T) {
		w := newFakeRunbookWorld()
		seed(w)
		w.mu.Lock()
		w.collections[w.collectionKey("gold", "configs")]["indexes"] = []any{}
		w.mu.Unlock()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookDBMethod("CreateIndex") {
				w.collections[w.collectionKey("gold", "configs")]["indexes"] = []any{
					map[string]any{"id": "key", "type": "unique", "attributes": []any{"k"}},
				}
				return fakeRunbookErr(runbookCodeAlreadyExists, "index already exists")
			}
			return nil
		}
		out, err := runReconcileAction(t, w, "create_index", decl)
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})
}

func toAnySlice[T any](items []T) []any {
	out := make([]any, len(items))
	for i, v := range items {
		out[i] = v
	}
	return out
}

// TestRunbookCallErrorChain：CallError 的 Unwrap 链保留错误分类可达性。
func TestRunbookCallErrorChain(t *testing.T) {
	inner := fakeRunbookErr(runbookCodeNotFound, "database not found")
	wrapped := fmt.Errorf("ctx: %w", inner)
	require.True(t, isRunbookCode(wrapped, runbookCodeNotFound))
	require.False(t, isRunbookCode(wrapped, runbookCodeAlreadyExists))
	require.False(t, isRunbookCode(errors.New("plain"), runbookCodeNotFound))
	require.Contains(t, wrapped.Error(), "database not found")
}
