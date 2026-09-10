package cmd

import (
	"flag"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// storageFlagsDecl 声明 storage 域动词涉及的旗标（供 presence 测试）。
func storageFlagsDecl(fs *flag.FlagSet) {
	fs.Bool("public", false, "")
	fs.String("name", "", "")
	fs.String("mime-type", "", "")
	fs.String("metadata", "", "")
}

func TestBuildCreateBucketReq(t *testing.T) {
	tests := []struct {
		name        string
		bucketName  string
		permissions string
		public      bool
		wantErr     string
	}{
		{name: "缺 name", wantErr: "--name is required"},
		{name: "最小字段", bucketName: "assets", wantErr: ""},
		{name: "公开桶", bucketName: "assets", public: true, permissions: `["read(\"all\")"]`, wantErr: ""},
		{name: "permissions 非法", bucketName: "assets", permissions: `x`, wantErr: "failed to parse --permissions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateBucketReq(tt.bucketName, tt.permissions, tt.public)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.bucketName, req["name"])
			require.Equal(t, tt.public, req["public"])
			if tt.permissions != "" {
				require.Equal(t, []string{`read("all")`}, req["permissions"])
			} else {
				_, ok := req["permissions"]
				require.False(t, ok, "permissions 未提供不应设置键: %v", req)
			}
		})
	}
}

func TestBuildUpdateBucketReq(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		newName string
		set     map[string]string
		wantErr string
	}{
		{name: "缺 id", wantErr: "missing bucket id"},
		{name: "仅改名", id: "b1", newName: "new", set: map[string]string{"name": "new"}, wantErr: ""},
		{name: "仅 public", id: "b1", set: map[string]string{"public": "true"}, wantErr: ""},
		{name: "全字段", id: "b1", newName: "new", set: map[string]string{"name": "new", "public": "false"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateBucketReq(newPresenceVerb(t, storageFlagsDecl, tt.set), tt.id, tt.newName, tt.set["public"] == "true")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.id, req["id"])
			_, hasName := req["name"]
			require.Equal(t, tt.newName != "", hasName, "name presence 不匹配: %v", req)
			if tt.newName != "" {
				require.Equal(t, tt.newName, req["name"])
			}
			_, wantPublic := tt.set["public"]
			_, hasPublic := req["public"]
			require.Equal(t, wantPublic, hasPublic, "public presence 不匹配: %v", req)
			if wantPublic {
				require.Equal(t, tt.set["public"] == "true", req["public"])
			}
		})
	}
}

func TestBuildUpdateFileReq(t *testing.T) {
	tests := []struct {
		name     string
		bucketID string
		fileID   string
		fileName string
		mimeType string
		metadata string
		set      map[string]string
		wantErr  string
	}{
		{name: "无可更新字段", bucketID: "b1", fileID: "f1", wantErr: "provide at least one of --name/--mime-type/--metadata"},
		{name: "仅改名", bucketID: "b1", fileID: "f1", fileName: "new.png", set: map[string]string{"name": "new.png"}, wantErr: ""},
		{name: "仅 metadata", bucketID: "b1", fileID: "f1", metadata: `{"author":"x"}`,
			set: map[string]string{"metadata": `{"author":"x"}`}, wantErr: ""},
		{name: "全字段", bucketID: "b1", fileID: "f1", fileName: "new.png", mimeType: "image/png",
			metadata: `{"author":"x"}`, set: map[string]string{"name": "new.png", "mime-type": "image/png", "metadata": `{"author":"x"}`}, wantErr: ""},
		{name: "metadata 非法", bucketID: "b1", fileID: "f1", metadata: `[1]`, set: map[string]string{"metadata": `[1]`}, wantErr: "failed to parse --metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateFileReq(newPresenceVerb(t, storageFlagsDecl, tt.set), tt.bucketID, tt.fileID, tt.fileName, tt.mimeType, tt.metadata)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.bucketID, req["bucketId"])
			require.Equal(t, tt.fileID, req["fileId"])
			_, hasName := req["name"]
			require.Equal(t, tt.fileName != "", hasName, "name presence 不匹配: %v", req)
			if tt.fileName != "" {
				require.Equal(t, tt.fileName, req["name"])
			}
			_, hasMime := req["mimeType"]
			require.Equal(t, tt.mimeType != "", hasMime, "mimeType presence 不匹配: %v", req)
			if tt.mimeType != "" {
				require.Equal(t, tt.mimeType, req["mimeType"])
			}
			if tt.metadata != "" {
				md, ok := req["metadata"].(map[string]string)
				require.True(t, ok, "metadata 未解析: %v", req)
				require.Equal(t, "x", md["author"])
			} else {
				_, ok := req["metadata"]
				require.False(t, ok, "metadata 未提供不应设置键: %v", req)
			}
		})
	}
}
