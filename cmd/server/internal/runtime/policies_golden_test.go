package runtime

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// 阶段 0 golden 基线（grpcapi 迁移设计 DESIGN.md §7，仓库 D:\Codes\qiulin\grpcapi）。
// 阶段 1 已切换：本测试现锁定 buildMethodPolicies（grpcapi.authz.Build +
// END_USER 归一 + AssertSemantic）的输出，与切换前实现字节级等价。
//
// 稳定 JSON 落盘，作为阶段 1 切换 grpcapi.authz.Build 的等价性证明基准与
// 回滚判据。投影刻意选择两套实现共有的语义面：
//   - admin_roles 以 domain string 形态记录（词表基准 = 现网持久化形态）；
//   - request_fields 从 descriptor 现算（现实现只有 RequestHasProjectID 布尔，
//     grpcapi.MethodPolicy.RequestFields 语义相同，阶段 1 后同一 golden 直接可比）；
//   - scope 词表记录 资源→已声明方向 派生面（死 scope 断言的对照面）。
//
// 基线更新（proto 策略有意的变更时）：
//
//	go test ./cmd/server/internal/runtime -run TestPolicyGoldenBaseline -update
//
// 预期外的红 = proto 策略漂移或收集器行为变化，先确认是否有意再更新基线。
var updateGolden = flag.Bool("update", false, "rewrite policy golden baseline")

type goldenScope struct {
	Resource string `json:"resource"`
	Op       string `json:"op"`
}

type goldenPolicy struct {
	Method              string       `json:"method"`
	Service             string       `json:"service"`
	Access              string       `json:"access"`
	Permissions         []string     `json:"permissions,omitempty"`
	AdminRoles          []string     `json:"admin_roles,omitempty"`
	Scope               *goldenScope `json:"scope,omitempty"`
	RequestFields       []string     `json:"request_fields"`
	RequestHasProjectID bool         `json:"request_has_project_id"`
	IsStreaming         bool         `json:"is_streaming"`
}

type goldenVocabEntry struct {
	Resource string   `json:"resource"`
	Ops      []string `json:"ops"`
}

type goldenFile struct {
	MethodCount     int                `json:"method_count"`
	ScopeVocabulary []goldenVocabEntry `json:"scope_vocabulary"`
	Methods         []goldenPolicy     `json:"methods"`
}

func accessName(a domainauth.AccessLevel) string {
	switch a {
	case domainauth.AccessPublic:
		return "public"
	case domainauth.AccessEndUser:
		return "end_user"
	case domainauth.AccessServer:
		return "server"
	case domainauth.AccessPermission:
		return "permission"
	case domainauth.AccessSystem:
		return "system"
	default:
		return fmt.Sprintf("unspecified(%d)", int(a))
	}
}

// requestFields 从输入消息 descriptor 现算排序全字段投影。
func requestFields(md protoreflect.MethodDescriptor) []string {
	fields := md.Input().Fields()
	out := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		out = append(out, string(fields.Get(i).Name()))
	}
	// 稳定排序：与 grpcapi.MethodPolicy.RequestFields 的字典序约定一致。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func buildGolden(t *testing.T) goldenFile {
	t.Helper()

	files := authzFileDescriptors()
	set, err := buildMethodPolicies()
	if err != nil {
		t.Fatalf("buildMethodPolicies: %v", err)
	}
	// 锁语义断言恒过（阶段 1 后项目钩子形态的 AssertSemantic 仍须过同栏）。
	if err := domainauth.AssertSemantic(set); err != nil {
		t.Fatalf("AssertSemantic: %v", err)
	}

	// method → 输入消息 descriptor（request_fields 现算）。
	inputs := make(map[string]protoreflect.MethodDescriptor)
	for _, fd := range files {
		services := fd.Services()
		for i := 0; i < services.Len(); i++ {
			svc := services.Get(i)
			methods := svc.Methods()
			for j := 0; j < methods.Len(); j++ {
				md := methods.Get(j)
				inputs[fmt.Sprintf("/%s/%s", svc.FullName(), md.Name())] = md
			}
		}
	}

	g := goldenFile{Methods: make([]goldenPolicy, 0, 32)}
	for _, p := range set.Methods() { // Methods() 已按方法名排序
		gp := goldenPolicy{
			Method:              p.Method,
			Service:             p.Service,
			Access:              accessName(p.Access),
			Permissions:         p.Permissions,
			RequestFields:       nil,
			RequestHasProjectID: slices.Contains(p.RequestFields, "project_id"),
			IsStreaming:         p.IsStreaming,
		}
		if len(p.Permissions) > 0 {
			gp.Permissions = p.Permissions
		}
		if len(p.AdminRoles) > 0 {
			gp.AdminRoles = make([]string, len(p.AdminRoles))
			for i, r := range p.AdminRoles {
				gp.AdminRoles[i] = string(r)
			}
		}
		if p.Scope != nil {
			gp.Scope = &goldenScope{Resource: string(p.Scope.Resource), Op: string(p.Scope.Op)}
		}
		if md, ok := inputs[p.Method]; ok {
			gp.RequestFields = requestFields(md)
		} else {
			t.Fatalf("method %s not found in authzFileDescriptors inputs", p.Method)
		}
		g.Methods = append(g.Methods, gp)
	}
	g.MethodCount = len(g.Methods)

	vocab := domainauth.VocabularyFromPolicies(set)
	for _, res := range vocab.Resources() {
		entry := goldenVocabEntry{Resource: string(res)}
		for _, op := range []domainauth.ScopeOp{domainauth.ScopeRead, domainauth.ScopeWrite, domainauth.ScopeAdmin} {
			if vocab.HasOp(res, op) {
				entry.Ops = append(entry.Ops, string(op))
			}
		}
		g.ScopeVocabulary = append(g.ScopeVocabulary, entry)
	}
	return g
}

func marshalGolden(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func unmarshalGolden(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func TestPolicyGoldenBaseline(t *testing.T) {
	g := buildGolden(t)

	path := filepath.Join("testdata", "policies.golden.json")

	if *updateGolden {
		data, err := marshalGolden(g)
		if err != nil {
			t.Fatalf("marshal golden: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden baseline rewritten: %s (%d methods)", path, g.MethodCount)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (首次生成请跑 go test ./cmd/server/internal/runtime -run TestPolicyGoldenBaseline -update): %v", err)
	}
	var want goldenFile
	if err := unmarshalGolden(data, &want); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	if want.MethodCount != g.MethodCount {
		t.Errorf("method count = %d, golden %d；方法集合漂移（新增/删除 RPC 未更新基线？）", g.MethodCount, want.MethodCount)
	}
	for i, got := range g.Methods {
		if i >= len(want.Methods) || got.Method != want.Methods[i].Method {
			t.Errorf("method 列表漂移于第 %d 项：got %q", i, got.Method)
			continue
		}
		gotJSON, errGot := marshalGolden(got)
		wantJSON, errWant := marshalGolden(want.Methods[i])
		if errGot != nil || errWant != nil {
			t.Fatalf("marshal: %v / %v", errGot, errWant)
		}
		// 字节级比较（marshalGolden 输出由字段序决定、确定性）；
		// 相同 JSON 不同内存形态（nil vs 空 slice 等）不构成策略漂移。
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("策略漂移 %s:\n got: %s\nwant: %s", got.Method, gotJSON, wantJSON)
		}
	}
	gotVocab, _ := marshalGolden(g.ScopeVocabulary)
	wantVocab, _ := marshalGolden(want.ScopeVocabulary)
	if string(gotVocab) != string(wantVocab) {
		t.Errorf("scope 词表漂移:\n got: %s\nwant: %s", gotVocab, wantVocab)
	}
}
