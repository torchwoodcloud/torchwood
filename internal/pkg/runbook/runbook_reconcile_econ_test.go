package runbook

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 阶段 D 动词 schema（静态表驱动；字段面 = 三个 proto 的 *Request 消息）
// ---------------------------------------------------------------------------

func TestValidateRunbookEconVerbs(t *testing.T) {
	load := func(content string) []runbookFile {
		t.Helper()
		dir := writeRunbookDir(t, map[string]string{"000001_x.yaml": content})
		files, err := loadRunbookDir(dir)
		require.NoError(t, err)
		return files
	}

	t.Run("三域八动词全字段面过", func(t *testing.T) {
		files := load(`up:
  - create_bucket: { name: gs, permissions: ['read:users'], public: true }
  - update_bucket: { name: gs, public: false }
  - delete_bucket: { name: gs }
  - create_asset_def:
      code: gold
      name: Gold
      class: currency
      decimals: 2
      max_quantity: 1000000
      expires_in: 3600
      tradable: true
      unique_per_owner: true
      upgradeable: false
      metadata: { rarity: epic }
  - update_asset_def: { code: gold, name: Goldy, decimals: 3, max_quantity: 10, expires_in: 60, tradable: false, unique_per_owner: false, upgradeable: true, metadata: { a: 1 }, status: active }
  - delete_asset_def: { code: gold }
  - create_leaderboard_board:
      id: arena
      sort: desc
      tiebreak_order: asc
      tie_break: parallel
      period_kind: weekly
      period_tz: UTC
      policy: best
      value_min: 0
      value_max: 100
      client_submit: true
      per_subject_submit_limit: 10
      retention_periods: 4
      subject_kind: user
  - update_leaderboard_board:
      board_id: arena
      sort: asc
      tiebreak_order: desc
      clear_tiebreak: true
      tie_break: earliest
      period_kind: daily
      period_tz: Asia/Shanghai
      policy: sum
      value_min: 1
      value_max: 99
      clear_value_bounds: true
      client_submit: false
      per_subject_submit_limit: 5
      retention_periods: 8
      subject_kind: team
down: []
`)
		require.NoError(t, validateRunbookVerbs(files))
	})

	t.Run("缺引用键 / 缺必填", func(t *testing.T) {
		for _, tc := range []struct{ yaml, want string }{
			{"up:\n  - create_bucket: { public: true }\n", `missing required field "name"`},
			{"up:\n  - update_bucket: { public: true }\n", `missing required field "name"`},
			{"up:\n  - delete_bucket: {}\n", `missing required field "name"`},
			{"up:\n  - create_asset_def: { name: Gold }\n", `missing required field "code"`},
			{"up:\n  - create_asset_def: { code: gold }\n", `missing required field "name"`},
			{"up:\n  - update_asset_def: { name: Goldy }\n", `missing required field "code"`},
			{"up:\n  - delete_asset_def: {}\n", `missing required field "code"`},
			{"up:\n  - create_leaderboard_board: { sort: desc }\n", `missing required field "id"`},
			{"up:\n  - update_leaderboard_board: { sort: asc }\n", `missing required field "board_id"`},
		} {
			err := validateRunbookVerbs(load(tc.yaml))
			require.Error(t, err, tc.yaml)
			require.Contains(t, err.Error(), tc.want, tc.yaml)
		}
	})

	t.Run("bucket 的 id 是引擎注入字段，YAML 侧报未知字段", func(t *testing.T) {
		err := validateRunbookVerbs(load("up:\n  - create_bucket: { name: gs, id: x }\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), `unknown field "id"`)
	})

	t.Run("metadata 非映射报错（google.protobuf.Struct）", func(t *testing.T) {
		err := validateRunbookVerbs(load("up:\n  - create_asset_def: { code: gold, name: Gold, class: currency, metadata: epic }\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be a mapping")
	})

	t.Run("board 动词未知字段", func(t *testing.T) {
		err := validateRunbookVerbs(load("up:\n  - create_leaderboard_board: { id: arena, rewards: [] }\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), `unknown field "rewards"`)
	})
}

// ---------------------------------------------------------------------------
// 分页列表（bucket / asset def 读取路径的公共底座）
// ---------------------------------------------------------------------------

func TestRunbookListAllPagesDrainsTokens(t *testing.T) {
	call := 0
	rec := &runbookReconciler{caller: func(method string, req map[string]any) (map[string]any, error) {
		require.Equal(t, runbookStorageMethod("ListBuckets"), method)
		require.EqualValues(t, 100, req["page_size"])
		tok, _ := req["page_token"].(string)
		call++
		page := func(ids []string, token string) map[string]any {
			items := make([]any, 0, len(ids))
			for _, id := range ids {
				items = append(items, map[string]any{"id": id})
			}
			return map[string]any{"buckets": items, "meta": map[string]any{"nextPageToken": token}}
		}
		switch call {
		case 1:
			require.Empty(t, tok, "首页不带 page_token")
			return page([]string{"a", "b"}, "t1"), nil
		case 2:
			require.Equal(t, "t1", tok)
			return page([]string{"c"}, "t2"), nil
		default:
			require.Equal(t, "t2", tok)
			return page(nil, ""), nil // 空页无 token：终止
		}
	}}
	out, err := rec.listAllPages(runbookStorageMethod("ListBuckets"), "buckets")
	require.NoError(t, err)
	require.Len(t, out, 3)
	require.Equal(t, "a", runbookGotStr(out[0], "id"))
	require.Equal(t, "c", runbookGotStr(out[2], "id"))
	require.Equal(t, 3, call, "token 跟随到排干")
}

// ---------------------------------------------------------------------------
// bucket：name 解析三分支 / create 比较 / TOCTOU / update / delete 按 name→id
// ---------------------------------------------------------------------------

func econBucket(name string) map[string]any {
	return map[string]any{"name": name, "permissions": []any{"read:users"}}
}

func (w *fakeRunbookWorld) econBucketByName(name string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.buckets {
		if b["name"] == name {
			return b
		}
	}
	return nil
}

func (w *fakeRunbookWorld) econAssetDef(code string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.assetDefs[code]
}

func fakeCallsFor(w *fakeRunbookWorld, method string) []fakeRunbookCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []fakeRunbookCall
	for _, c := range w.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func TestReconcileBucketBranches(t *testing.T) {
	t.Run("create 分支：不存在 → 建", func(t *testing.T) {
		w := newFakeRunbookWorld()
		out, err := runReconcileAction(t, w, "create_bucket", econBucket("gold-store"))
		require.NoError(t, err)
		require.Equal(t, "created", out.Report.Result)
		require.Equal(t, "gold-store", out.Report.Target)
		b := w.econBucketByName("gold-store")
		require.NotNil(t, b)
		require.Equal(t, []any{"read:users"}, b["permissions"])
	})

	t.Run("skip 分支：存在且相等（permissions 乱序等价，D20）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.buckets["bkt_0001"] = map[string]any{
			"id": "bkt_0001", "name": "gold-store",
			"permissions": []any{"read:users"}, "public": false,
		}
		w.mu.Unlock()
		out, err := runReconcileAction(t, w, "create_bucket", econBucket("gold-store"))
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
		require.Len(t, w.econBucketByName("gold-store")["permissions"].([]any), 1)
	})

	t.Run("fail 分支：public 不等（带 diff）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.buckets["bkt_0001"] = map[string]any{
			"id": "bkt_0001", "name": "gold-store",
			"permissions": []any{"read:users"}, "public": true,
		}
		w.mu.Unlock()
		_, err := runReconcileAction(t, w, "create_bucket", econBucket("gold-store"))
		require.Error(t, err)
		var drift *runbookDriftError
		require.ErrorAs(t, err, &drift)
		require.Contains(t, err.Error(), "public")
	})

	t.Run("permissions 不等出 diff（排序归一后）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.buckets["bkt_0001"] = map[string]any{
			"id": "bkt_0001", "name": "gold-store",
			"permissions": []any{"read:keys"}, "public": false,
		}
		w.mu.Unlock()
		_, err := runReconcileAction(t, w, "create_bucket", econBucket("gold-store"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "permissions")
	})

	t.Run("name 多命中 → fail 人工裁决（0 命中按不存在在 create 分支覆盖）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.mu.Lock()
		w.buckets["b1"] = map[string]any{"id": "b1", "name": "dup", "permissions": []any{}, "public": false}
		w.buckets["b2"] = map[string]any{"id": "b2", "name": "dup", "permissions": []any{}, "public": false}
		w.mu.Unlock()
		_, err := runReconcileAction(t, w, "create_bucket", econBucket("dup"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "ambiguous")
		require.Contains(t, err.Error(), "b1")
		require.Contains(t, err.Error(), "b2")
	})

	t.Run("TOCTOU：撞 AlreadyExists 后重读相等 → skip", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookStorageMethod("CreateBucket") {
				w.buckets["bkt_0001"] = map[string]any{
					"id": "bkt_0001", "name": "gold-store",
					"permissions": []any{"read:users"}, "public": false,
				}
				return fakeRunbookErr(runbookCodeAlreadyExists, "bucket already exists")
			}
			return nil
		}
		out, err := runReconcileAction(t, w, "create_bucket", econBucket("gold-store"))
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("TOCTOU：撞后重读不等 → fail 带 diff", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookStorageMethod("CreateBucket") {
				w.buckets["bkt_0001"] = map[string]any{
					"id": "bkt_0001", "name": "gold-store",
					"permissions": []any{"read:users"}, "public": true,
				}
				return fakeRunbookErr(runbookCodeAlreadyExists, "bucket already exists")
			}
			return nil
		}
		_, err := runReconcileAction(t, w, "create_bucket", econBucket("gold-store"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "public")
	})

	t.Run("update：按 name→id 调 RPC，体直传", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_bucket", econBucket("gold-store"))
		require.NoError(t, err)
		out, err := runReconcileAction(t, w, "update_bucket", map[string]any{"name": "gold-store", "public": true})
		require.NoError(t, err)
		require.Equal(t, "updated", out.Report.Result)
		b := w.econBucketByName("gold-store")
		require.Equal(t, true, b["public"])
		// RPC 体：id 注入 + name/public presence 直传。
		calls := fakeCallsFor(w, runbookStorageMethod("UpdateBucket"))
		require.Len(t, calls, 1)
		require.Equal(t, "bkt_0001", calls[0].Req["id"])
		require.Equal(t, "gold-store", calls[0].Req["name"])
		require.Equal(t, true, calls[0].Req["public"])
	})

	t.Run("update：0 命中 = NotFound 分类错误（退出码映射同 update_collection）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "update_bucket", map[string]any{"name": "nope", "public": true})
		require.Error(t, err)
		require.True(t, isRunbookCode(err, runbookCodeNotFound))
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("delete：存在 → deleted（按解析 id）；不存在 → skipped", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_bucket", econBucket("gold-store"))
		require.NoError(t, err)
		out, err := runReconcileAction(t, w, "delete_bucket", map[string]any{"name": "gold-store"})
		require.NoError(t, err)
		require.Equal(t, "deleted", out.Report.Result)
		w.mu.Lock()
		require.Empty(t, w.buckets)
		w.mu.Unlock()
		calls := fakeCallsFor(w, runbookStorageMethod("DeleteBucket"))
		require.Len(t, calls, 1)
		require.Equal(t, "bkt_0001", calls[0].Req["id"])

		out, err = runReconcileAction(t, w, "delete_bucket", map[string]any{"name": "gold-store"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})
}

// ---------------------------------------------------------------------------
// asset def：code 反查 / D12 白名单（含类别矩阵镜像与 optional presence）/
// D10 复活 / delete 归档
// ---------------------------------------------------------------------------

func econGoldDef() map[string]any {
	return map[string]any{
		"code": "gold", "name": "Gold", "class": "currency",
		"decimals": 2, "max_quantity": 1000000,
	}
}

// seedEconAssetDef 以读回侧键形（camelCase）置入一个 def。
func (w *fakeRunbookWorld) seedEconAssetDef(overlay map[string]any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	code := overlay["code"].(string)
	d := map[string]any{"id": "def_" + code, "code": code, "status": "active"}
	for k, v := range overlay {
		d[k] = v
	}
	w.assetDefs[code] = d
}

func TestReconcileAssetDefBranches(t *testing.T) {
	t.Run("create 分支：不存在 → 建（currency 矩阵 forcing 落库）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		out, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.NoError(t, err)
		require.Equal(t, "created", out.Report.Result)
		require.Equal(t, "gold", out.Report.Target)
		d := w.econAssetDef("gold")
		require.Equal(t, "active", d["status"])
		require.Equal(t, true, d["uniquePerOwner"], "currency → unique_per_owner 天然 true")
		require.Equal(t, "def_gold", d["id"])
	})

	t.Run("skip 分支：active 且相等（forcing 镜像，省略 unique_per_owner 不误报）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.NoError(t, err)
		out, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("fail 分支：active 不等（decimals 出 diff）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.seedEconAssetDef(map[string]any{
			"code": "gold", "name": "Gold", "class": "currency",
			"decimals": 8, "maxQuantity": 1000000, "uniquePerOwner": true,
		})
		_, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.Error(t, err)
		var drift *runbookDriftError
		require.ErrorAs(t, err, &drift)
		require.Contains(t, err.Error(), "decimals")
	})

	t.Run("optional presence：声明缺 max_quantity 与读回带值出 diff（<absent> 渲染）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.seedEconAssetDef(map[string]any{
			"code": "gold", "name": "Gold", "class": "currency",
			"decimals": 2, "maxQuantity": 42, "uniquePerOwner": true,
		})
		body := econGoldDef()
		delete(body, "max_quantity") // 声明侧未设置
		_, err := runReconcileAction(t, w, "create_asset_def", body)
		require.Error(t, err)
		require.Contains(t, err.Error(), "max_quantity")
		require.Contains(t, err.Error(), "<absent>")
	})

	t.Run("optional presence：两侧都未设置等价", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.seedEconAssetDef(map[string]any{
			"code": "gold", "name": "Gold", "class": "currency",
			"decimals": 2, "uniquePerOwner": true,
		})
		body := econGoldDef()
		delete(body, "max_quantity")
		out, err := runReconcileAction(t, w, "create_asset_def", body)
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})

	t.Run("metadata 相等（JSON 规范形）与不等出 diff", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.seedEconAssetDef(map[string]any{
			"code": "gem", "name": "Gem", "class": "instance",
			"metadata": map[string]any{"rarity": "epic", "tier": 3},
		})
		same := map[string]any{
			"code": "gem", "name": "Gem", "class": "instance",
			"metadata": map[string]any{"tier": 3, "rarity": "epic"}, // 键序无关
		}
		out, err := runReconcileAction(t, w, "create_asset_def", same)
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)

		w.seedEconAssetDef(map[string]any{
			"code": "gem", "name": "Gem", "class": "instance",
			"metadata": map[string]any{"rarity": "legendary"},
		})
		_, err = runReconcileAction(t, w, "create_asset_def", same)
		require.Error(t, err)
		require.Contains(t, err.Error(), "metadata")
	})

	t.Run("entitlement forcing 镜像：tradable 声明被服务端压 false，双跑不误报", func(t *testing.T) {
		w := newFakeRunbookWorld()
		body := map[string]any{
			"code": "vip", "name": "VIP", "class": "entitlement", "tradable": true,
		}
		_, err := runReconcileAction(t, w, "create_asset_def", body)
		require.NoError(t, err)
		d := w.econAssetDef("vip")
		require.Equal(t, false, d["tradable"], "服务端 forcing 压 false")
		out, err := runReconcileAction(t, w, "create_asset_def", body)
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result, "want 侧镜像 forcing，不误报漂移")
	})

	t.Run("设计文档示例的 CLASS_CURRENCY 写法被服务端拒绝（class 是 string 非枚举）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		body := map[string]any{"code": "gold", "name": "Gold", "class": "CLASS_CURRENCY"}
		_, err := runReconcileAction(t, w, "create_asset_def", body)
		require.Error(t, err)
		require.True(t, isRunbookCode(err, "InvalidArgument"))
		require.Contains(t, err.Error(), "invalid class")
	})

	t.Run("D10 复活：归档 + 相等（除 status）→ UpdateAssetDef(status=active)", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.seedEconAssetDef(map[string]any{
			"code": "gold", "name": "Gold", "class": "currency",
			"decimals": 2, "maxQuantity": 1000000, "uniquePerOwner": true,
			"status": "archived",
		})
		out, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.NoError(t, err)
		require.Equal(t, "restored", out.Report.Result)
		require.Equal(t, "active", w.econAssetDef("gold")["status"])
		calls := fakeCallsFor(w, runbookAssetsMethod("UpdateAssetDef"))
		require.Len(t, calls, 1, "复活走 UpdateAssetDef")
		require.Equal(t, "def_gold", calls[0].Req["def_id"])
		require.Equal(t, "active", calls[0].Req["status"])
	})

	t.Run("D10：归档 + 不等 → fail 带 diff（status 不在白名单）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.seedEconAssetDef(map[string]any{
			"code": "gold", "name": "Gold Coin", "class": "currency",
			"decimals": 2, "maxQuantity": 1000000, "uniquePerOwner": true,
			"status": "archived",
		})
		_, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.Error(t, err)
		require.Contains(t, err.Error(), "name")
		require.NotContains(t, err.Error(), `field "status"`, "status 不参与比较")
		require.Equal(t, "archived", w.econAssetDef("gold")["status"], "不等不复活")
	})

	t.Run("TOCTOU：CreateAssetDef 撞归档占位行 → 重读复活", func(t *testing.T) {
		w := newFakeRunbookWorld()
		w.intercept = func(w *fakeRunbookWorld, method string, req map[string]any) error {
			if method == runbookAssetsMethod("CreateAssetDef") {
				w.assetDefs["gold"] = map[string]any{
					"id": "def_gold", "code": "gold", "name": "Gold", "class": "currency",
					"decimals": 2, "maxQuantity": 1000000, "uniquePerOwner": true,
					"status": "archived",
				}
				return fakeRunbookErr(runbookCodeAlreadyExists, "asset def already exists")
			}
			return nil
		}
		out, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.NoError(t, err)
		require.Equal(t, "restored", out.Report.Result)
	})

	t.Run("update：code 反查 def_id，剥离 code 直传其余字段", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.NoError(t, err)
		out, err := runReconcileAction(t, w, "update_asset_def", map[string]any{
			"code": "gold", "name": "Gold Bars",
		})
		require.NoError(t, err)
		require.Equal(t, "updated", out.Report.Result)
		require.Equal(t, "Gold Bars", w.econAssetDef("gold")["name"])
		calls := fakeCallsFor(w, runbookAssetsMethod("UpdateAssetDef"))
		require.Len(t, calls, 1)
		require.Equal(t, "def_gold", calls[0].Req["def_id"])
		require.NotContains(t, calls[0].Req, "code", "code 不是 UpdateAssetDefRequest 字段")
		require.Equal(t, "Gold Bars", calls[0].Req["name"])
	})

	t.Run("update：未知 code = NotFound 分类错误", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "update_asset_def", map[string]any{"code": "nope", "name": "X"})
		require.Error(t, err)
		require.True(t, isRunbookCode(err, runbookCodeNotFound))
	})

	t.Run("delete：active → 归档；已归档 → skipped；不存在 → skipped", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_asset_def", econGoldDef())
		require.NoError(t, err)

		out, err := runReconcileAction(t, w, "delete_asset_def", map[string]any{"code": "gold"})
		require.NoError(t, err)
		require.Equal(t, "deleted", out.Report.Result)
		require.Equal(t, "archived", w.econAssetDef("gold")["status"], "DeleteAssetDef = 归档软删")
		calls := fakeCallsFor(w, runbookAssetsMethod("DeleteAssetDef"))
		require.Len(t, calls, 1)
		require.Equal(t, "def_gold", calls[0].Req["def_id"])

		out, err = runReconcileAction(t, w, "delete_asset_def", map[string]any{"code": "gold"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result, "已归档 = 目标态")

		out, err = runReconcileAction(t, w, "delete_asset_def", map[string]any{"code": "nope"})
		require.NoError(t, err)
		require.Equal(t, "skipped", out.Report.Result)
	})
}

// ---------------------------------------------------------------------------
// board：直传服务端原生幂等 provisioning（比较在服务端，D12）
// ---------------------------------------------------------------------------

func TestReconcileLeaderboardBoard(t *testing.T) {
	arenaDecl := func() map[string]any {
		return map[string]any{"id": "arena", "sort": "desc", "period_kind": "weekly"}
	}

	t.Run("create 直传 → created（缺省归一落库）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		out, err := runReconcileAction(t, w, "create_leaderboard_board", arenaDecl())
		require.NoError(t, err)
		require.Equal(t, "created", out.Report.Result)
		require.Equal(t, "arena", out.Report.Target)
		w.mu.Lock()
		b := w.boards["arena"]
		w.mu.Unlock()
		require.NotNil(t, b)
		require.Equal(t, "desc", b["sort"])
		require.Equal(t, "weekly", b["periodKind"])
		require.EqualValues(t, 100, b["perSubjectSubmitLimit"], "缺省归一（applyCreateDefaults 镜像）")
		require.Equal(t, "best", b["policy"])
		require.Equal(t, "parallel", b["tieBreak"])
	})

	t.Run("重放相等 → 2xx 收敛（服务端不区分新建与重放，引擎统一报 created）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_leaderboard_board", arenaDecl())
		require.NoError(t, err)
		out, err := runReconcileAction(t, w, "create_leaderboard_board", arenaDecl())
		require.NoError(t, err)
		require.Equal(t, "created", out.Report.Result)
	})

	t.Run("缺省等价：显式声明缺省值与省略等价（不误报）", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_leaderboard_board", map[string]any{"id": "arena"})
		require.NoError(t, err)
		_, err = runReconcileAction(t, w, "create_leaderboard_board", map[string]any{
			"id": "arena", "sort": "desc", "tie_break": "parallel", "policy": "best",
			"period_kind": "none", "per_subject_submit_limit": 100, "subject_kind": "user",
		})
		require.NoError(t, err, "缺省归一后逐字段相等 → 200")
	})

	t.Run("配置不等 → AlreadyExists 透传服务端字段 diff", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_leaderboard_board", arenaDecl())
		require.NoError(t, err)
		_, err = runReconcileAction(t, w, "create_leaderboard_board", map[string]any{
			"id": "arena", "sort": "asc", "period_kind": "weekly",
		})
		require.Error(t, err)
		require.True(t, isRunbookCode(err, runbookCodeAlreadyExists), "保留 RPC 分类与退出码映射")
		require.Contains(t, err.Error(), "already exists with different config")
		require.Contains(t, err.Error(), "sort: requested asc, existing desc")
	})

	t.Run("optional 差异透传：value_min requested <unset>, existing 5", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_leaderboard_board", map[string]any{
			"id": "arena", "value_min": 5,
		})
		require.NoError(t, err)
		_, err = runReconcileAction(t, w, "create_leaderboard_board", map[string]any{"id": "arena"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "value_min: requested <unset>, existing 5")
	})

	t.Run("update 按 presence 直传：只写出现的键", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_leaderboard_board", arenaDecl())
		require.NoError(t, err)
		out, err := runReconcileAction(t, w, "update_leaderboard_board", map[string]any{
			"board_id": "arena", "retention_periods": 12,
		})
		require.NoError(t, err)
		require.Equal(t, "updated", out.Report.Result)
		w.mu.Lock()
		b := w.boards["arena"]
		w.mu.Unlock()
		require.EqualValues(t, 12, b["retentionPeriods"])
		require.Equal(t, "desc", b["sort"], "未写的键不动")
		require.Equal(t, "weekly", b["periodKind"])
	})

	t.Run("update：clear_value_bounds 清空上下界", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "create_leaderboard_board", map[string]any{
			"id": "arena", "value_min": 5, "value_max": 99,
		})
		require.NoError(t, err)
		_, err = runReconcileAction(t, w, "update_leaderboard_board", map[string]any{
			"board_id": "arena", "clear_value_bounds": true,
		})
		require.NoError(t, err)
		w.mu.Lock()
		b := w.boards["arena"]
		w.mu.Unlock()
		require.NotContains(t, b, "valueMin")
		require.NotContains(t, b, "valueMax")
	})

	t.Run("update：未知榜 NotFound 分类透传", func(t *testing.T) {
		w := newFakeRunbookWorld()
		_, err := runReconcileAction(t, w, "update_leaderboard_board", map[string]any{
			"board_id": "ghost", "sort": "asc",
		})
		require.Error(t, err)
		require.True(t, isRunbookCode(err, runbookCodeNotFound))
	})
}

// ---------------------------------------------------------------------------
// 全链路：三域 runbook 的 up → down → up 循环
// ---------------------------------------------------------------------------

const runbookFixtureEconBucket = `up:
  - create_bucket:
      name: gold-store
      permissions: ['read:users']
down:
  - delete_bucket:
      name: gold-store
`

// 设计文档 §2.1 的 000002 文件（class 值勘误见测试内注释）。
const runbookFixtureEconGoldAsset = `up:
  - create_asset_def:
      code: gold
      name: Gold
      class: currency
      decimals: 2
      max_quantity: 1000000
down:
  - delete_asset_def:
      code: gold          # 服务端语义为归档（soft），引擎复活语义见 D10
`

const runbookFixtureEconBoard = `up:
  - create_leaderboard_board:
      id: arena
      sort: desc
      period_kind: weekly
  - update_leaderboard_board:
      board_id: arena
      retention_periods: 12
down:
  # 服务端无 DeleteLeaderboardBoard：down 只做形状回退（撤销 up 的 update），
  # 榜本身保留——down 总则：资源形状回退，不承诺资源删除。
  - update_leaderboard_board:
      board_id: arena
      retention_periods: 0
`

// writeEconFixtureDir 落盘三域迁移目录。gold 资产示例即设计文档 §2.1 的
// 000002 文件；唯一偏差是 class 值用小写 currency——proto 字段是 string 而
// 非枚举，domain Class 常量为小写（currency/stack/instance/entitlement），
// 设计示例的 CLASS_CURRENCY 写法会被服务端矩阵校验拒绝。
func writeEconFixtureDir(t *testing.T) string {
	t.Helper()
	return writeRunbookDir(t, map[string]string{
		"000001_create_gold_store.yaml":  runbookFixtureEconBucket,
		"000002_create_gold_asset.yaml":  runbookFixtureEconGoldAsset,
		"000003_create_arena_board.yaml": runbookFixtureEconBoard,
	})
}

func econActionResult(t *testing.T, step StepReport, verb string) string {
	t.Helper()
	for _, a := range step.Actions {
		if a.Verb == verb {
			return a.Result
		}
	}
	t.Fatalf("step %06d has no action %q", step.Version, verb)
	return ""
}

func TestRunbookEconFullCycle(t *testing.T) {
	w := newFakeRunbookWorld()
	dir := writeEconFixtureDir(t)

	// up：三域全部落地。
	summary, err := runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(3), summary.CurrentVersion)
	require.Len(t, summary.Applied, 3)
	require.Equal(t, "created", econActionResult(t, summary.Applied[0], "create_bucket"))
	require.Equal(t, "created", econActionResult(t, summary.Applied[1], "create_asset_def"))
	require.Equal(t, "created", econActionResult(t, summary.Applied[2], "create_leaderboard_board"))
	require.Equal(t, "updated", econActionResult(t, summary.Applied[2], "update_leaderboard_board"))

	w.mu.Lock()
	require.Len(t, w.buckets, 1)
	gold := w.assetDefs["gold"]
	arena := w.boards["arena"]
	w.mu.Unlock()
	require.NotNil(t, w.econBucketByName("gold-store"))
	require.Equal(t, "active", gold["status"])
	require.EqualValues(t, 1000000, gold["maxQuantity"])
	require.EqualValues(t, 12, arena["retentionPeriods"])

	// 双跑：状态守卫下无事可做。
	summary, err = runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Empty(t, summary.Applied)

	// down --all：bucket 删除、gold 归档、board 形状回退（榜保留）。reverted
	// 按 down 顺序从顶版起记录：[v3 board, v2 gold, v1 bucket]。
	downSummary, err := runDownOnWorld(t, w, dir, func(o *RunOptions) { o.All = true })
	require.NoError(t, err)
	require.Equal(t, int64(0), downSummary.CurrentVersion)
	require.Len(t, downSummary.Reverted, 3)
	require.Equal(t, "updated", econActionResult(t, downSummary.Reverted[0], "update_leaderboard_board"))
	require.Equal(t, "deleted", econActionResult(t, downSummary.Reverted[1], "delete_asset_def"))
	require.Equal(t, "deleted", econActionResult(t, downSummary.Reverted[2], "delete_bucket"))
	w.mu.Lock()
	arena = w.boards["arena"]
	require.Empty(t, w.buckets, "down 删除 bucket")
	w.mu.Unlock()
	require.Equal(t, "archived", w.econAssetDef("gold")["status"], "down 的 delete = 归档")
	require.NotNil(t, arena, "服务端无 Delete：榜保留")
	require.EqualValues(t, 0, arena["retentionPeriods"], "形状回退：retention 归零")

	// 再 up：bucket 重建、gold 经 D10 复活、board 2xx 重放 + update 重放。
	summary, err = runUpOnWorld(t, w, dir, nil)
	require.NoError(t, err)
	require.Equal(t, int64(3), summary.CurrentVersion)
	require.Equal(t, "created", econActionResult(t, summary.Applied[0], "create_bucket"))
	require.Equal(t, "restored", econActionResult(t, summary.Applied[1], "create_asset_def"), "D10 复活闭合 down→up 循环")
	require.Equal(t, "created", econActionResult(t, summary.Applied[2], "create_leaderboard_board"), "provisioning 重放 2xx")
	require.Equal(t, "updated", econActionResult(t, summary.Applied[2], "update_leaderboard_board"))
	require.Equal(t, "active", w.econAssetDef("gold")["status"])
	require.NotNil(t, w.econBucketByName("gold-store"))

	// status 对账干净。
	statusOut := runStatusOnWorld(t, w, dir)
	require.True(t, statusOut.Clean)
	require.Equal(t, int64(3), statusOut.CurrentVersion)
	require.Equal(t, "applied", statusOut.Files[0].State)
	require.False(t, statusOut.Files[0].Irreversible)
	require.False(t, statusOut.Files[2].Irreversible, "board step 的 down 是形状回退，非空即可逆")
}
