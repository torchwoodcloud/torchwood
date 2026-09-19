package functions

import (
	"testing"
)

// 运行时表不变量护栏（docs/design/functions-runtime-selection.md §1）：
// 表是 app/runner/dispatcher 三方的单一事实源，半截状态（表里有 ID、模板
// 没基座、缺缺省项）在这里拦下。
func TestRuntimeTableInvariants(t *testing.T) {
	rs := Runtimes()
	if len(rs) == 0 {
		t.Fatal("runtime table must not be empty")
	}

	seen := map[string]bool{}
	defaults := 0
	for _, r := range rs {
		if r.ID == "" {
			t.Fatal("runtime ID must not be empty")
		}
		if seen[r.ID] {
			t.Fatalf("duplicate runtime ID %q", r.ID)
		}
		seen[r.ID] = true

		switch r.Family {
		case RuntimeFamilyNode, RuntimeFamilyGo, RuntimeFamilyImage, RuntimeFamilyPython:
		default:
			t.Fatalf("runtime %q has unknown family %q", r.ID, r.Family)
		}

		switch r.Status {
		case RuntimeStatusActive, RuntimeStatusDeprecated, RuntimeStatusEOL:
		default:
			t.Fatalf("runtime %q has unknown status %q", r.ID, r.Status)
		}

		// active/deprecated 表项必须带基座镜像（可构建）；eol 允许留空基座
		// （不再新构建），但当前实现全部保留以便补构建对照。image family
		// 平台零构建，恒空。
		if r.Family != RuntimeFamilyImage && r.Status != RuntimeStatusEOL && r.BaseImage == "" {
			t.Fatalf("runtime %q (status %q) must declare a base image", r.ID, r.Status)
		}

		if r.IsDefault {
			defaults++
			if r.Status != RuntimeStatusActive {
				t.Fatalf("default runtime %q must be active", r.ID)
			}
		}
	}

	if defaults != 1 {
		t.Fatalf("exactly one default runtime expected, got %d", defaults)
	}
}

func TestRuntimeLookup(t *testing.T) {
	if _, ok := RuntimeByID("node-24.0"); !ok {
		t.Fatal("node-24.0 must be in the table")
	}
	if _, ok := RuntimeByID("node-20.0"); ok {
		t.Fatal("node-20.0 (EOL upstream) must not be added as a runtime")
	}
	if got := FamilyOf("node-18.0"); got != RuntimeFamilyNode {
		t.Fatalf("FamilyOf(node-18.0) = %q, want node", got)
	}
	if got := FamilyOf("deno-2.0"); got != "" {
		t.Fatalf("FamilyOf(unknown) = %q, want empty", got)
	}

	// 缺省 runtime = node 家族首个 active 表项（node-24.0）。
	def, ok := DefaultRuntimeForFamily(RuntimeFamilyNode)
	if !ok || def.ID != "node-24.0" || !def.IsDefault {
		t.Fatalf("DefaultRuntimeForFamily(node) = %+v ok=%v, want node-24.0", def, ok)
	}

	// python 家族无表项（runner 未实现）——遗留空声明路径必须显式失败。
	if _, ok := DefaultRuntimeForFamily(RuntimeFamilyPython); ok {
		t.Fatal("python family must have no default runtime")
	}

	// node-18.0 标 eol（2025-04-30 上游 EOL——可跑、拒新建的事实进表）。
	old, ok := RuntimeByID("node-18.0")
	if !ok {
		t.Fatal("node-18.0 must remain in the table (旧 ID 不日落)")
	}
	if old.Status != RuntimeStatusEOL || old.EolAt == nil {
		t.Fatalf("node-18.0 must be marked eol with EolAt, got %+v", old)
	}
}
