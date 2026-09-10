package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 配置文件位于 ~/.torchwood/config.yaml（TORCHWOOD_CLI_CONFIG 覆盖路径），
// 单文件承载多 profile：一个 profile 对应一个项目上下文（endpoint + 该项目
// 的 scoped API Key），多 project 各占一个 profile 实现配置隔离——Agent 只需
// --profile <name> 即可操作对应项目，API Key 本体不进对话/脚本。
const (
	envConfig  = "TORCHWOOD_CLI_CONFIG"
	envProfile = "TORCHWOOD_CLI_PROFILE"
)

// profileNameRe 约束 profile 名：字母/数字开头，含 _ 和 -，≤64 字符。
// 名字只出现在配置文件与旗标里（不做文件名），收紧是为了防止手滑写出
// 不可打的名字。
var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// cliConfig 是配置文件的磁盘模型。
type cliConfig struct {
	// Default 是未指定 --profile / TORCHWOOD_CLI_PROFILE 时生效的 profile。
	Default string `yaml:"default,omitempty"`
	// Profiles 按 profile 名索引；指针统一非 nil（load 时校验）。
	Profiles map[string]*profile `yaml:"profiles,omitempty"`
}

// profile 是单个项目上下文的连接与输出配置；除 endpoint / api-key 外均可选。
type profile struct {
	Endpoint string `yaml:"endpoint,omitempty"`
	APIKey   string `yaml:"api-key,omitempty"`
	TLS      bool   `yaml:"tls,omitempty"`
	Timeout  string `yaml:"timeout,omitempty"`
	Output   string `yaml:"output,omitempty"`
}

// configPath 解析配置文件路径：TORCHWOOD_CLI_CONFIG 优先，否则 ~/.torchwood/config.yaml。
// HOME 无法定位时返回错误（RPC 路径会静默跳过配置，管理命令如实报错）。
func configPath() (string, error) {
	if p := os.Getenv(envConfig); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".torchwood", "config.yaml"), nil
}

// loadConfigFile 读取并解析配置文件；文件不存在返回 (nil, nil)。解析用
// KnownFields 严格模式：拼错的键（如 api_key）直接报错而不是静默忽略，
// 否则「设了不生效」比「报错」更难排查。
func loadConfigFile(path string) (*cliConfig, error) {
	// path 来自 configPath()（HOME 或 TORCHWOOD_CLI_CONFIG），非不可信输入
	data, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg cliConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// validate 校验配置内容：profile 名合法、字段值可解析、default 指向存在的
// profile（悬空 default 按错处理而非降级——静默回落内建端点会让用户在
// 以为连着生产时实际打到 localhost）。
func (c *cliConfig) validate() error {
	for name, p := range c.Profiles {
		if !profileNameRe.MatchString(name) {
			return fmt.Errorf("profile name %q is invalid (letters/digits/'_'/'-', starting with a letter or digit, max 64 chars)", name)
		}
		if p == nil {
			return fmt.Errorf("profile %q has no fields", name)
		}
		if p.Timeout != "" {
			if _, err := time.ParseDuration(p.Timeout); err != nil {
				return fmt.Errorf("profile %q: invalid timeout %q: %v", name, p.Timeout, err)
			}
		}
		if p.Output != "" && p.Output != "json" {
			return fmt.Errorf("profile %q: unsupported output %q (json only)", name, p.Output)
		}
	}
	if c.Default != "" {
		if _, ok := c.Profiles[c.Default]; !ok {
			return fmt.Errorf("default profile %q does not exist (profiles: %s)", c.Default, c.profileList())
		}
	}
	return nil
}

// profileList 返回排序的 profile 名逗号串（错误提示与列表用）。
func (c *cliConfig) profileList() string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// saveConfigFile 以 0600 写回配置文件（目录 0700）；写前再跑一次 validate，
// 保证任何写路径都不会落盘非法配置。yaml.v3 对 map 键按字典序输出，文件
// 形状确定。
func saveConfigFile(path string, cfg *cliConfig) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("invalid config %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

// configTemplate 是 config init 落盘的初始内容（带注释的人读模板；
// 必须能被 loadConfigFile 的严格模式解析）。
const configTemplate = `# Torchwood CLI config (path: ~/.torchwood/config.yaml, override via TORCHWOOD_CLI_CONFIG).
# One profile = one project context: an endpoint plus that project's scoped API key.
# Profile selection: --profile flag > TORCHWOOD_CLI_PROFILE > the "default" key below.
# Field precedence:  explicit flag > TORCHWOOD_CLI_* env var > profile value > built-in default.
default: local
profiles:
  local:
    endpoint: 127.0.0.1:9060
    api-key: ""            # paste the API key here, or: torchwood config set local api-key <secret>
    # tls: false           # set true when a reverse proxy terminates TLS (forwards h2c)
    # timeout: 30s
    # output: json
`

// saveConfigFileRaw 原样写入配置文件内容（config init 的模板带注释，不经
// 结构体 marshal）；权限与目录语义与 saveConfigFile 一致。
func saveConfigFileRaw(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

// loadConfigOrDefault 供 config list/show 使用：文件不存在时返回 (path, nil, nil)，
// 由调用方决定空态呈现。
func loadConfigOrDefault() (string, *cliConfig, error) {
	path, err := configPath()
	if err != nil {
		return "", nil, err
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		return path, nil, err
	}
	if cfg == nil {
		return path, nil, nil
	}
	return path, cfg, nil
}

// sortedProfileNames 返回排序的 profile 名。
func sortedProfileNames(cfg *cliConfig) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// profileJSON 是 config list/show 的 JSON 视图：API Key 一律打码，任何
// 管理命令的输出都不回显密钥本体。
type profileJSON struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"apiKey"`
	TLS      bool   `json:"tls"`
	Timeout  string `json:"timeout"`
	Output   string `json:"output"`
}

func newProfileJSON(name string, p *profile) profileJSON {
	return profileJSON{
		Name:     name,
		Endpoint: p.Endpoint,
		APIKey:   maskAPIKey(p.APIKey),
		TLS:      p.TLS,
		Timeout:  p.Timeout,
		Output:   p.Output,
	}
}

// printJSONIndent 以缩进 JSON 输出本地对象（RPC 响应走 printJSON 原样渲染）。
func printJSONIndent(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// trimSpaceBytes 去掉首尾空白（config set --stdin 读入的值带换行）。
func trimSpaceBytes(b []byte) string {
	return strings.TrimSpace(string(b))
}

// mutateConfigFile 是管理命令共用的写路径：加载 → 变更 → 校验保存。
// 文件不存在时以空配置起步（config set 可直接建出首个 profile）。
func mutateConfigFile(fn func(cfg *cliConfig) error) (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		return path, err
	}
	if cfg == nil {
		cfg = &cliConfig{Profiles: map[string]*profile{}}
	}
	if err := fn(cfg); err != nil {
		return path, err
	}
	if err := saveConfigFile(path, cfg); err != nil {
		return path, err
	}
	return path, nil
}

// setProfileField 把 <key>=<value> 写入指定 profile（不存在则创建）；
// 值的合法性在这里就地校验，非法值不落盘。
func setProfileField(cfg *cliConfig, name, key, value string) (created bool, err error) {
	if !profileNameRe.MatchString(name) {
		return false, fmt.Errorf("profile name %q is invalid (letters/digits/'_'/'-', starting with a letter or digit, max 64 chars)", name)
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		p = &profile{}
		cfg.Profiles[name] = p
		created = true
	}
	switch key {
	case "endpoint":
		p.Endpoint = value
	case "api-key":
		p.APIKey = value
	case "tls":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return created, fmt.Errorf("invalid %s value %q: must be true or false", key, value)
		}
		p.TLS = b
	case "timeout":
		if _, err := time.ParseDuration(value); err != nil {
			return created, fmt.Errorf("invalid %s value %q: %v", key, value, err)
		}
		p.Timeout = value
	case "output":
		if value != "json" {
			return created, fmt.Errorf("invalid %s value %q: json only", key, value)
		}
		p.Output = value
	default:
		return created, fmt.Errorf("unknown key %q (valid keys: endpoint, api-key, tls, timeout, output)", key)
	}
	return created, nil
}

// maskAPIKey 把 API Key 打码为末 4 位（长度不足 12 时不泄露任何片段）。
func maskAPIKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) < 12 {
		return "****"
	}
	return "****" + k[len(k)-4:]
}
