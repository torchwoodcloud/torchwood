package config

import (
	"strings"

	"github.com/lynx-go/lynx"
	"github.com/spf13/pflag"
)

const EnvPrefix = "TORCHWOOD"

func ConfigureConfigSource(f *pflag.FlagSet, c lynx.ConfigSource, extraPaths ...string) error {
	if err := lynx.DefaultBindConfigFunc(f, c); err != nil {
		return err
	}

	for _, path := range extraPaths {
		if path == "" {
			continue
		}
		c.AddSearchPath(path)
	}

	// 与旧版 viper.SetEnvKeyReplacer 语义一致：任意点号/连字符键都能被
	// TORCHWOOD_* 形式的环境变量覆盖（"data.database.source" →
	// "TORCHWOOD_DATA_DATABASE_SOURCE"）。
	c.SetEnvPrefix(EnvPrefix)
	c.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	c.AutomaticEnv()

	return nil
}

func NewBindConfigFunc(extraPaths ...string) lynx.BindConfigFunc {
	return func(f *pflag.FlagSet, c lynx.ConfigSource) error {
		return ConfigureConfigSource(f, c, extraPaths...)
	}
}

// UnmarshalConfig 把 lynx.Config 解码到 AppConfig。AppConfig 为 proto 生成
// 结构体（json tag、snake_case 键）；lynx v1.17 起 Unmarshal 默认走结构体
// 驱动的逐叶取值（tag 回退链 mapstructure → json → 小写字段名），且仅在
// 环境变量中设置的键也参与解码，无需额外选项。
func UnmarshalConfig(c lynx.Config, out *AppConfig) error {
	return c.Unmarshal(out)
}
