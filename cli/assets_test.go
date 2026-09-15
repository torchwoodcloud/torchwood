package cli

import (
	"encoding/json"
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildCreateDefReq(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		defName  string
		class    string
		metadata string
		wantErr  string
		want     map[string]any
	}{
		{name: "缺 code", defName: "金币", class: "currency", wantErr: "--code, --name and --class are required"},
		{name: "缺 name", code: "gold", class: "currency", wantErr: "--code, --name and --class are required"},
		{name: "缺 class", code: "gold", defName: "金币", wantErr: "--code, --name and --class are required"},
		{name: "最小配置", code: "gold", defName: "金币", class: "currency",
			want: map[string]any{"code": "gold", "name": "金币", "class": "currency"}},
		{name: "全配置", code: "gold", defName: "金币", class: "currency", metadata: `{"skin":"default"}`,
			want: map[string]any{"code": "gold", "name": "金币", "class": "currency", "metadata": map[string]any{"skin": "default"}}},
		{name: "metadata 非法 JSON", code: "gold", defName: "金币", class: "currency", metadata: `{bad`,
			wantErr: "failed to parse --metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateDefReq(tt.code, tt.defName, tt.class, 2, 100, 3600, true, false, true, tt.metadata)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			for k, v := range tt.want {
				require.Equal(t, v, req[k], "键 %s", k)
			}
			require.Equal(t, 2, req["decimals"])
			require.Equal(t, int64(100), req["maxQuantity"])
			require.Equal(t, int64(3600), req["expiresIn"])
			require.Equal(t, true, req["tradable"])
			_, ok := req["uniquePerOwner"]
			require.False(t, ok, "false 布尔不应写键")
			require.Equal(t, true, req["upgradeable"])
		})
	}
}

func TestBuildUpdateDefReq(t *testing.T) {
	tests := []struct {
		name    string
		defID   string
		set     map[string]string
		wantErr string
		want    map[string]any
	}{
		{name: "缺 def-id", wantErr: "missing def-id"},
		{name: "零改动", defID: "d1", want: map[string]any{"defId": "d1"}},
		{name: "改 status", defID: "d1", set: map[string]string{"status": "archived"},
			want: map[string]any{"defId": "d1", "status": "archived"}},
		{name: "布尔显式 false 生效", defID: "d1", set: map[string]string{"tradable": "false"},
			want: map[string]any{"defId": "d1", "tradable": false}},
		{name: "metadata 仅显式传入", defID: "d1", set: map[string]string{"metadata": `{"a":1}`},
			want: map[string]any{"defId": "d1", "metadata": map[string]any{"a": json.Number("1")}}},
		{name: "数值 presence", defID: "d1", set: map[string]string{"decimals": "4"},
			want: map[string]any{"defId": "d1", "decimals": 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newPresenceVerb(t, func(fs *flag.FlagSet) {
				fs.String("name", "", "")
				fs.Int("decimals", 0, "")
				fs.Int64("max-quantity", 0, "")
				fs.Int64("expires-in", 0, "")
				fs.Bool("tradable", false, "")
				fs.Bool("unique-per-owner", false, "")
				fs.Bool("upgradeable", false, "")
				fs.String("metadata", "", "")
				fs.String("status", "", "")
			}, tt.set)
			req, err := buildUpdateDefReq(v, tt.defID, defPatch{
				name: tt.set["name"], decimals: testInt(tt.set["decimals"]),
				maxQuantity: testInt64(tt.set["max-quantity"]), expiresIn: testInt64(tt.set["expires-in"]),
				tradable: testBool(tt.set["tradable"]), uniquePerOwner: testBool(tt.set["unique-per-owner"]),
				upgradeable: testBool(tt.set["upgradeable"]), metadata: tt.set["metadata"], status: tt.set["status"],
			})
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, req)
		})
	}
}

func TestBuildGrantReq(t *testing.T) {
	tests := []struct {
		name      string
		ownerID   string
		defCode   string
		quantity  int64
		expiresAt string
		metadata  string
		wantErr   string
		wantKeys  []string
	}{
		{name: "缺 owner", defCode: "gold", quantity: 1, wantErr: "missing owner-id/def-code"},
		{name: "缺 def-code", ownerID: "u1", quantity: 1, wantErr: "missing owner-id/def-code"},
		{name: "缺 quantity", ownerID: "u1", defCode: "gold", wantErr: "--quantity is required"},
		{name: "负 quantity", ownerID: "u1", defCode: "gold", quantity: -1, wantErr: "--quantity is required"},
		{name: "全字段（日期归一）", ownerID: "u1", defCode: "gold", quantity: 5, expiresAt: "2026-12-31",
			metadata: `{"src":"admin"}`, wantKeys: []string{"ownerId", "defCode", "quantity", "expiresAt", "metadata", "level"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildGrantReq(tt.ownerID, tt.defCode, tt.quantity, "k1", tt.expiresAt, 3, tt.metadata, "order", "o9")
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "2026-12-31T00:00:00Z", req["expiresAt"])
			require.Equal(t, map[string]any{"src": "admin"}, req["metadata"])
			require.Equal(t, 3, req["level"])
			require.Equal(t, int64(5), req["quantity"])
			require.Equal(t, "k1", req["idempotencyKey"])
			require.Equal(t, "order", req["refType"])
			require.Equal(t, "o9", req["refId"])
		})
	}
}

func TestBuildMutateReq(t *testing.T) {
	noChange := func() *verb {
		return newPresenceVerb(t, func(fs *flag.FlagSet) {
			fs.Int("level", 0, "")
			fs.String("expires-at", "", "")
			fs.String("metadata", "", "")
		}, nil)
	}
	t.Run("缺 holding-id", func(t *testing.T) {
		_, err := buildMutateReq(noChange(), "", "", 0, "", "", "", "")
		require.ErrorContains(t, err, "missing holding-id")
	})
	t.Run("无修改目标拒绝", func(t *testing.T) {
		_, err := buildMutateReq(noChange(), "h1", "", 0, "", "", "", "")
		require.ErrorContains(t, err, "nothing to mutate")
	})
	t.Run("level presence 生效", func(t *testing.T) {
		v := newPresenceVerb(t, func(fs *flag.FlagSet) {
			fs.Int("level", 0, "")
			fs.String("expires-at", "", "")
			fs.String("metadata", "", "")
		}, map[string]string{"level": "2", "expires-at": "2027-01-01"})
		req, err := buildMutateReq(v, "h1", "k1", 2, "2027-01-01", "", "admin", "op-1")
		require.NoError(t, err)
		require.Equal(t, 2, req["level"])
		require.Equal(t, "2027-01-01T00:00:00Z", req["expiresAt"])
		_, ok := req["metadata"]
		require.False(t, ok, "未显式传 --metadata 不应设置键")
		require.Equal(t, "k1", req["idempotencyKey"])
		require.Equal(t, "admin", req["refType"])
	})
	t.Run("metadata 非法 JSON", func(t *testing.T) {
		v := newPresenceVerb(t, func(fs *flag.FlagSet) {
			fs.Int("level", 0, "")
			fs.String("expires-at", "", "")
			fs.String("metadata", "", "")
		}, map[string]string{"metadata": "{"})
		_, err := buildMutateReq(v, "h1", "", 0, "", "{", "", "")
		require.ErrorContains(t, err, "failed to parse --metadata")
	})
}

func TestBuildConsumeAndTransferReq(t *testing.T) {
	_, err := buildConsumeReq("u1", "", 1, "", "", "")
	require.ErrorContains(t, err, "missing owner-id/def-code")
	_, err = buildConsumeReq("u1", "gold", 0, "", "", "")
	require.ErrorContains(t, err, "--quantity is required")
	req, err := buildConsumeReq("u1", "gold", 2, "k", "quest", "q1")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"ownerId": "u1", "defCode": "gold", "quantity": int64(2),
		"idempotencyKey": "k", "refType": "quest", "refId": "q1"}, req)

	_, err = buildTransferReq("", "u2", "gold", 1, "", "", "")
	require.ErrorContains(t, err, "missing from-owner-id/to-owner-id/def-code")
	req, err = buildTransferReq("u1", "u2", "gold", 1, "", "", "")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"fromOwnerId": "u1", "toOwnerId": "u2", "defCode": "gold",
		"quantity": int64(1)}, req)
}
