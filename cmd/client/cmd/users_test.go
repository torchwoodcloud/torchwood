package cmd

import (
	"encoding/json"
	"flag"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildCreateUserReq(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		password string
		nameArg  string
		status   string
		data     string
		wantErr  string
	}{
		{name: "必填校验", email: "", password: "", wantErr: "--email 与 --password 必填"},
		{name: "最小字段", email: "a@b.c", password: "pw", wantErr: ""},
		{name: "全字段", email: "a@b.c", password: "pw", nameArg: "Alice", status: "active", wantErr: ""},
		{name: "data 合并 labels", email: "a@b.c", password: "pw", data: `{"labels":{"x":1}}`, wantErr: ""},
		{name: "data 非法 JSON", email: "a@b.c", password: "pw", data: `{invalid`, wantErr: "--data 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateUserReq(tt.email, tt.password, tt.nameArg, tt.status, tt.data)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.email, req["email"])
			require.Equal(t, tt.password, req["password"])
			if tt.nameArg != "" {
				require.Equal(t, tt.nameArg, req["name"])
			}
			if tt.status != "" {
				require.Equal(t, tt.status, req["status"])
			}
			if tt.data != "" {
				labels, ok := req["labels"].(map[string]any)
				require.True(t, ok, "labels 未合并: %v", req)
				require.Equal(t, json.Number("1"), labels["x"])
			}
		})
	}
}

func TestBuildUpdateUserReq(t *testing.T) {
	newVerbFor := func(setEmailVerified bool) *verb {
		set := map[string]string{}
		if setEmailVerified {
			set["email-verified"] = "true"
		}
		return newPresenceVerb(t, func(fs *flag.FlagSet) {
			fs.Bool("email-verified", false, "")
		}, set)
	}
	tests := []struct {
		name          string
		id            string
		emailVerified bool
		nameArg       string
		email         string
		status        string
		data          string
		wantErr       string
		wantVerified  bool
	}{
		{name: "缺 id", wantErr: "缺少用户 ID"},
		{name: "仅 id", id: "u1", wantErr: ""},
		{name: "email-verified 未显式设置", id: "u1", wantErr: ""},
		{name: "email-verified 显式设置", id: "u1", emailVerified: true, wantVerified: true, wantErr: ""},
		{name: "全字段", id: "u1", emailVerified: true, nameArg: "Bob", email: "b@c.d", status: "inactive",
			wantVerified: true, wantErr: ""},
		{name: "data 合并 prefs", id: "u1", data: `{"prefs":{"theme":"dark"}}`, wantErr: ""},
		{name: "data 非法", id: "u1", data: `{`, wantErr: "--data 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateUserReq(newVerbFor(tt.emailVerified), tt.id, tt.emailVerified, tt.nameArg, tt.email, tt.status, tt.data)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			if !tt.emailVerified {
				_, ok := req["emailVerified"]
				require.False(t, ok, "未显式传 --email-verified 不应设置键: %v", req)
			} else {
				require.Equal(t, tt.wantVerified, req["emailVerified"])
			}
			if tt.nameArg == "" {
				_, ok := req["name"]
				require.False(t, ok, "未传 --name 不应设置 name: %v", req)
			} else {
				require.Equal(t, tt.nameArg, req["name"])
			}
			if tt.data == "" {
				_, ok := req["prefs"]
				require.False(t, ok, "未传 --data 不应设置 prefs: %v", req)
			}
		})
	}
}

func TestListJSON(t *testing.T) {
	tests := []struct {
		name      string
		pageSize  int
		pageToken string
		wantKeys  []string
	}{
		{name: "默认分页", wantKeys: nil},
		{name: "带分页参数", pageSize: 10, pageToken: "tok", wantKeys: []string{"pageSize", "pageToken"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := listJSON(tt.pageSize, tt.pageToken)
			for _, k := range tt.wantKeys {
				_, ok := m[k]
				require.True(t, ok, "缺少键 %s: %v", k, m)
			}
			for k := range m {
				require.Contains(t, tt.wantKeys, k, "多余键 %s: %v", k, m)
			}
		})
	}
}

func TestJSONInt64MapPrecision(t *testing.T) {
	m, err := jsonInt64Map(`{"big": 1234567890123456789}`, "--increment")
	require.NoError(t, err)
	require.Equal(t, json.Number("1234567890123456789"), m["big"]) // 不经 float64
}
