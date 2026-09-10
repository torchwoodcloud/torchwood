package functionsdispatcher

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// newSpawnLockToken 生成 spawn 锁持有者令牌（比对删除防误删他人锁）。
func newSpawnLockToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退化用时间戳仍可保唯一性（同进程内）。
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
