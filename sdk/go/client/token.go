package client

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// TokenStore 持久化 TokenBundle（access/refresh token）。
type TokenStore interface {
	// Load 返回已存 token；无 token 时返回 (nil, nil)。
	Load() (*clientv1.TokenBundle, error)
	Save(*clientv1.TokenBundle) error
	// Clear 删除已存 token；无 token 时不报错。
	Clear() error
}

// MemoryTokenStore 进程内 TokenStore。
type MemoryTokenStore struct {
	mu sync.Mutex
	t  *clientv1.TokenBundle
}

// NewMemoryTokenStore 创建空的内存 TokenStore。
func NewMemoryTokenStore() *MemoryTokenStore { return &MemoryTokenStore{} }

func (s *MemoryTokenStore) Load() (*clientv1.TokenBundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t, nil
}

func (s *MemoryTokenStore) Save(t *clientv1.TokenBundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t = t
	return nil
}

func (s *MemoryTokenStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t = nil
	return nil
}

// FileTokenStore JSON 文件 TokenStore：protojson 格式、0600 权限、
// 临时文件 + rename 原子写，内置 mutex 可并发使用。
type FileTokenStore struct {
	mu   sync.Mutex
	path string
}

// NewFileTokenStore 创建绑定 path 的文件 TokenStore；路径开头的 ~ 会展开为
// 用户主目录（仅支持 ~ 与 ~/ 形式，不支持 ~user/；Save 时自动创建父目录，0700）。
func NewFileTokenStore(path string) *FileTokenStore { return &FileTokenStore{path: expandHome(path)} }

// expandHome 展开路径开头的 ~ 与 ~/ 为用户主目录。
// 仅支持这两种形式；"~user/" 等其他形式不支持，原样返回。
// Windows 上同时接受 "~/" 与 "~\"（用户习惯两种写法）。
func expandHome(path string) string {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return path
		}
		return home
	}
	if !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	return filepath.Join(home, path[2:])
}

func (s *FileTokenStore) Load() (*clientv1.TokenBundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("torchwood: read token file: %w", err)
	}
	var t clientv1.TokenBundle
	if err := protojson.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("torchwood: parse token file: %w", err)
	}
	return &t, nil
}

func (s *FileTokenStore) Save(t *clientv1.TokenBundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := protojson.Marshal(t)
	if err != nil {
		return fmt.Errorf("torchwood: encode token: %w", err)
	}
	if dir := filepath.Dir(s.path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("torchwood: create token dir: %w", err)
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("torchwood: write token file: %w", err)
	}
	// Windows 上 os.Rename 目标已存在会报错，先移除旧文件（忽略 NotExist）；
	// POSIX 直接 rename 原子替换，不开文件缺失窗口。
	if runtime.GOOS == "windows" {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("torchwood: remove old token file: %w", err)
		}
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("torchwood: rename token file: %w", err)
	}
	// 确保已存在文件也被收紧权限（rename 继承 tmp 权限，此处兜底）
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("torchwood: chmod token file: %w", err)
	}
	return nil
}

func (s *FileTokenStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("torchwood: remove token file: %w", err)
	}
	return nil
}
