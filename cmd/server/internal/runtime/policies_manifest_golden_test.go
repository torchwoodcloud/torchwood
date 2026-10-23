package runtime

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// updateGolden 重写 golden 基线（proto 策略有意的变更时）：
//
//	go test ./cmd/server/internal/runtime -run TestPolicyManifestGolden -update
//
// 预期外的红 = proto 策略漂移或收集器行为变化，先确认是否有意再更新基线。
var updateGolden = flag.Bool("update", false, "rewrite policy golden baseline")

// TestPolicyManifestGolden 锁定 v3 规范树的 manifest 快照
// （policy.Set.Manifest().JSON()）：路径 B codemod（2026-10-23）后策略声明
// 直读 policy.proto（protopolicy），树形状/kind/values/match/label 全量
// 可见，供此后策略漂移的 CI diff。codemod 等价性验收已完成（树与
// legacyProvider 展开逐字节一致；阶段 0 五档投影基线随之退役删除）。
func TestPolicyManifestGolden(t *testing.T) {
	set, err := buildMethodPolicies()
	if err != nil {
		t.Fatalf("buildMethodPolicies: %v", err)
	}
	if err := domainauth.AssertSemantic(set); err != nil {
		t.Fatalf("AssertSemantic: %v", err)
	}
	got, err := set.Manifest().JSON()
	if err != nil {
		t.Fatalf("manifest json: %v", err)
	}

	path := filepath.Join("testdata", "policies.manifest.golden.json")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("write manifest golden: %v", err)
		}
		t.Logf("manifest golden rewritten: %s", path)
		return
	}

	want, err := os.ReadFile(path) //nolint:gosec // 测试内固定的 testdata 相对路径
	if err != nil {
		t.Fatalf("read manifest golden（首次生成请跑 go test ./cmd/server/internal/runtime -run TestPolicyManifestGolden -update）: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Errorf("v3 树 manifest 漂移（策略树形状/kind/values 变更需显式 review 并 -update）:\ndiff 摘要见 testdata/policies.manifest.golden.json")
	}
}
