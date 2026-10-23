package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// TestPolicyManifestGolden 锁定 v3 规范树的 manifest 快照（policy.Set.
// Manifest().JSON()，MIGRATION §2.5 golden manifest 等价性验收的第二基线）：
//   - testdata/policies.golden.json        —— 阶段 0 五档投影基线（迁移前
//     buildMethodPolicies 输出固化），本迁移全程零漂移，即「新树 ≡ 旧投影」
//     的字节级等价证明；
//   - testdata/policies.manifest.golden.json —— v3 树自身的稳定快照，供此后
//     策略漂移的 CI diff（tree 形状/kind/values/match 全量可见）。
//
// 基线更新（proto 策略有意的变更时）：
//
//	go test ./cmd/server/internal/runtime -run TestPolicyManifestGolden -update
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
