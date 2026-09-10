package cmd

import (
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// funcFlagsDecl 声明 functions 域动词涉及的旗标（供 presence 测试）。
func funcFlagsDecl(fs *flag.FlagSet) {
	fs.Int("timeout-seconds", 0, "")
	fs.String("spec", "", "")
	fs.Bool("enabled", false, "")
	fs.String("name", "", "")
	fs.String("entrypoint", "", "")
	fs.String("deployment-id", "", "")
	fs.Bool("async", false, "")
}

func TestBuildCreateFunctionReq(t *testing.T) {
	tests := []struct {
		name           string
		id             string
		functionName   string
		runtime        string
		entrypoint     string
		timeoutSeconds int
		spec           string
		enabled        bool
		set            map[string]string
		wantErr        string
	}{
		{name: "缺 id", functionName: "f", runtime: "nodejs18", wantErr: "--id 必填"},
		{name: "缺 name", id: "f1", runtime: "nodejs18", wantErr: "--name 必填"},
		{name: "缺 runtime", id: "f1", functionName: "f", wantErr: "--runtime 必填"},
		{name: "最小字段", id: "f1", functionName: "f", runtime: "nodejs18", wantErr: ""},
		{name: "全字段", id: "f1", functionName: "f", runtime: "nodejs18", entrypoint: "index.js",
			timeoutSeconds: 30, spec: "shared-2x", enabled: true,
			set: map[string]string{"timeout-seconds": "30", "spec": "shared-2x", "enabled": "true"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateFunctionReq(newPresenceVerb(t, funcFlagsDecl, tt.set), tt.id, tt.functionName, tt.runtime, tt.entrypoint,
				tt.timeoutSeconds, tt.spec, tt.enabled)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.id, req["id"])
			require.Equal(t, tt.functionName, req["name"])
			require.Equal(t, tt.runtime, req["runtime"])
			if tt.entrypoint != "" {
				require.Equal(t, tt.entrypoint, req["entrypoint"])
			} else {
				_, ok := req["entrypoint"]
				require.False(t, ok, "entrypoint 未提供不应设置键: %v", req)
			}
			_, wantTimeout := tt.set["timeout-seconds"]
			_, hasTimeout := req["timeoutSeconds"]
			require.Equal(t, wantTimeout, hasTimeout, "timeoutSeconds presence 不匹配: %v", req)
			if wantTimeout {
				require.Equal(t, tt.timeoutSeconds, req["timeoutSeconds"])
			}
			_, wantSpec := tt.set["spec"]
			_, hasSpec := req["spec"]
			require.Equal(t, wantSpec, hasSpec, "spec presence 不匹配: %v", req)
			if wantSpec {
				require.Equal(t, tt.spec, req["spec"])
			}
			_, wantEnabled := tt.set["enabled"]
			_, hasEnabled := req["enabled"]
			require.Equal(t, wantEnabled, hasEnabled, "enabled presence 不匹配: %v", req)
			if wantEnabled {
				require.Equal(t, tt.enabled, req["enabled"])
			}
		})
	}
}

func TestBuildUpdateFunctionReq(t *testing.T) {
	tests := []struct {
		name           string
		functionID     string
		newName        string
		entrypoint     string
		timeoutSeconds int
		spec           string
		enabled        bool
		set            map[string]string
		wantErr        string
	}{
		{name: "缺 function-id", wantErr: "缺少 function-id"},
		{name: "仅 name", functionID: "f1", newName: "new", set: map[string]string{"name": "new"}, wantErr: ""},
		{name: "全字段", functionID: "f1", newName: "new", entrypoint: "main.py",
			timeoutSeconds: 60, spec: "shared-1x", enabled: false,
			set: map[string]string{"name": "new", "entrypoint": "main.py", "timeout-seconds": "60", "spec": "shared-1x", "enabled": "false"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateFunctionReq(newPresenceVerb(t, funcFlagsDecl, tt.set), tt.functionID, tt.newName, tt.entrypoint,
				tt.timeoutSeconds, tt.spec, tt.enabled)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.functionID, req["functionId"])
			_, wantName := tt.set["name"]
			_, hasName := req["name"]
			require.Equal(t, wantName, hasName, "name presence 不匹配: %v", req)
			if wantName {
				require.Equal(t, tt.newName, req["name"])
			}
			_, wantEntry := tt.set["entrypoint"]
			_, hasEntry := req["entrypoint"]
			require.Equal(t, wantEntry, hasEntry, "entrypoint presence 不匹配: %v", req)
			if wantEntry {
				require.Equal(t, tt.entrypoint, req["entrypoint"])
			}
			_, wantTimeout := tt.set["timeout-seconds"]
			_, hasTimeout := req["timeoutSeconds"]
			require.Equal(t, wantTimeout, hasTimeout, "timeoutSeconds presence 不匹配: %v", req)
			if wantTimeout {
				require.Equal(t, tt.timeoutSeconds, req["timeoutSeconds"])
			}
			_, wantSpec := tt.set["spec"]
			_, hasSpec := req["spec"]
			require.Equal(t, wantSpec, hasSpec, "spec presence 不匹配: %v", req)
			if wantSpec {
				require.Equal(t, tt.spec, req["spec"])
			}
			_, wantEnabled := tt.set["enabled"]
			_, hasEnabled := req["enabled"]
			require.Equal(t, wantEnabled, hasEnabled, "enabled presence 不匹配: %v", req)
			if wantEnabled {
				require.Equal(t, tt.enabled, req["enabled"])
			}
		})
	}
}

func TestBuildCreateDeploymentReq(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "code.zip")
	if err := os.WriteFile(good, []byte("PK\x03\x04fakezip"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		functionID string
		codePath   string
		wantErr    string
	}{
		{name: "缺 function-id", codePath: good, wantErr: "缺少 function-id"},
		{name: "缺 code", functionID: "f1", wantErr: "--code 必填"},
		{name: "文件不存在", functionID: "f1", codePath: filepath.Join(dir, "nope.zip"),
			wantErr: "读取 --code 失败"},
		{name: "正常", functionID: "f1", codePath: good, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateDeploymentReq(tt.functionID, tt.codePath)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, base64.StdEncoding.EncodeToString([]byte("PK\x03\x04fakezip")), req["code"])
		})
	}
}

func TestBuildSetVariablesReq(t *testing.T) {
	tests := []struct {
		name       string
		functionID string
		vars       string
		wantErr    string
	}{
		{name: "缺 vars", functionID: "f1", wantErr: "--vars 必填"},
		{name: "vars 非法", functionID: "f1", vars: `[1]`, wantErr: "--vars 解析失败"},
		{name: "正常", functionID: "f1", vars: `{"FOO":"bar"}`, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildSetVariablesReq(tt.functionID, tt.vars)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.functionID, req["functionId"])
			require.Equal(t, []map[string]string{{"key": "FOO", "value": "bar"}}, req["variables"])
		})
	}
}

func TestBuildCreateExecutionReq(t *testing.T) {
	tests := []struct {
		name         string
		functionID   string
		input        string
		deploymentID string
		async        bool
		set          map[string]string
		wantErr      string
	}{
		{name: "缺 input", functionID: "f1", wantErr: "--input 必填"},
		{name: "同步最小字段", functionID: "f1", input: `{"a":1}`, wantErr: ""},
		{name: "异步指定部署", functionID: "f1", input: `{}`, deploymentID: "d1", async: true,
			set: map[string]string{"deployment-id": "d1", "async": "true"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateExecutionReq(newPresenceVerb(t, funcFlagsDecl, tt.set), tt.functionID, tt.input, tt.deploymentID, tt.async)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.functionID, req["functionId"])
			require.Equal(t, tt.input, req["data"])
			_, wantDep := tt.set["deployment-id"]
			_, hasDep := req["deploymentId"]
			require.Equal(t, wantDep, hasDep, "deploymentId presence 不匹配: %v", req)
			if wantDep {
				require.Equal(t, tt.deploymentID, req["deploymentId"])
			}
			_, wantAsync := tt.set["async"]
			_, hasAsync := req["async"]
			require.Equal(t, wantAsync, hasAsync, "async presence 不匹配: %v", req)
			if wantAsync {
				require.Equal(t, tt.async, req["async"])
			}
		})
	}
}
