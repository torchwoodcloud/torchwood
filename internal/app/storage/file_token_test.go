package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fileTokenTestStorage(t *testing.T) *Storage {
	t.Helper()
	cfg := &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "test-master-secret-for-file-token"},
		},
	}
	return &Storage{cfg: cfg}
}

func rawMasterToken(master, projectID, bucketID, fileID string, expiresAt int64) string {
	mac := hmac.New(sha256.New, []byte(master))
	_, _ = fmt.Fprintf(mac, "%s.%s.%s.%d", projectID, bucketID, fileID, expiresAt)
	return fmt.Sprintf("%d.%s.%s.%s.%s", expiresAt, projectID, bucketID, fileID, hex.EncodeToString(mac.Sum(nil)))
}

// TestFileToken_PurposeKeySeparation 验证 file token 与 JWT 主密钥域分离：
// 旧行为（主密钥原文签名）签发的 token 必须无法通过 ParseFileToken，
// 派生 purpose key 签发的可解析，其他 purpose 的密钥同样无效。
func TestFileToken_PurposeKeySeparation(t *testing.T) {
	s := fileTokenTestStorage(t)
	master := s.cfg.GetSecurity().GetJwt().GetSecret()
	expiresAt := time.Now().Add(time.Hour).Unix()

	// 旧行为：主密钥原文为 HMAC key → 拒绝。
	legacy := rawMasterToken(master, "p1", "b1", "f1", expiresAt)
	pid, bid, fid, err := s.ParseFileToken(legacy)
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	require.Empty(t, pid)
	require.Empty(t, bid)
	require.Empty(t, fid)

	// 新行为：file-token purpose 派生密钥 → 解析成功且绑定字段还原。
	tok := signFileToken(jwtparser.DeriveKey(master, jwtparser.PurposeFileToken), "p1", "b1", "f1", expiresAt)
	pid, bid, fid, err = s.ParseFileToken(tok)
	require.NoError(t, err)
	require.Equal(t, "p1", pid)
	require.Equal(t, "b1", bid)
	require.Equal(t, "f1", fid)

	// 其他 purpose（end-user-jwt）密钥签发的 token → 拒绝。
	other := signFileToken(jwtparser.DeriveKey(master, jwtparser.PurposeEndUserJWT), "p1", "b1", "f1", expiresAt)
	_, _, _, err = s.ParseFileToken(other)
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// 伪造：同格式但 HMAC 内容不符 → 拒绝。
	forged := fmt.Sprintf("%d.p1.b1.f1.%s", expiresAt, hex.EncodeToString(make([]byte, 32)))
	_, _, _, err = s.ParseFileToken(forged)
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestFileToken_MissingSecret(t *testing.T) {
	s := &Storage{cfg: &config.AppConfig{}}
	_, _, _, err := s.ParseFileToken("1.2.3.4.5")
	require.Equal(t, codes.Internal, status.Code(err))
}

// TestCheckUploadOwner 上传会话 owner 校验规则：
// 空 owner（API key 创建）不约束；owner 本人通过；非 owner 端用户拒绝；
// keys/system/platform-admin 豁免。
func TestCheckUploadOwner(t *testing.T) {
	user1 := databases.Principal{Roles: []string{"users", "user:user-1"}}
	user2 := databases.Principal{Roles: []string{"users", "user:user-2"}}
	keys := databases.Principal{Roles: []string{"keys"}}
	admin := databases.Principal{PlatformAdmin: true}

	// 空 owner：不校验（API key 创建的会话走 scope + 项目归属门禁）。
	noOwner := &domainstorage.UploadSession{OwnerUserID: ""}
	require.NoError(t, checkUploadOwner(noOwner, "", keys))
	require.NoError(t, checkUploadOwner(noOwner, "any-user", user2))

	session := &domainstorage.UploadSession{OwnerUserID: "user-1"}
	// owner 本人通过。
	require.NoError(t, checkUploadOwner(session, "user-1", user1))
	// 非 owner 端用户拒绝。
	err := checkUploadOwner(session, "user-2", user2)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	// keys / system / platform-admin 豁免。
	require.NoError(t, checkUploadOwner(session, "", keys))
	require.NoError(t, checkUploadOwner(session, "", databases.SystemPrincipal))
	require.NoError(t, checkUploadOwner(session, "", admin))
}
