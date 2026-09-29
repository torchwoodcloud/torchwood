package dispatcher

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
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

// containerName 生成可读任务名：tw-fn-<project>-<function>-<rand6>。
// 与镜像逻辑名同族的可读性——任务视图一眼对上项目/函数。
// project/function 上游已过 ID 白名单（project ^[a-z][a-z0-9]{0,27}$、
// function ^[a-z0-9][a-z0-9_-]{0,63}$），此处防御性小写化 + 非法字符折叠为
// '-'；随机后缀（6 hex）保证同函数多实例/反复部署唯一。历史遗留大写
// functionID 同步小写（G6-3 同口径）。
func containerName(projectID, functionID string) string {
	sanitize := func(s string) string {
		s = strings.ToLower(s)
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
				return r
			default:
				return '-'
			}
		}, s)
	}
	suffix := newSpawnLockToken()[:6]
	return fmt.Sprintf("tw-fn-%s-%s-%s", sanitize(projectID), sanitize(functionID), suffix)
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
