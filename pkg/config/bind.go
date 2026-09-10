package config

import (
	"reflect"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/lynx-go/lynx"
	"github.com/spf13/pflag"
)

const EnvPrefix = "TORCHWOOD"

// envNameForKey 把点号路径键名映射为环境变量名，例如
// "data.database.source" -> "TORCHWOOD_DATA_DATABASE_SOURCE"。
func envNameForKey(key string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return strings.ToUpper(replacer.Replace(EnvPrefix + "_" + key))
}

func ConfigureViper(f *pflag.FlagSet, c lynx.ConfigSource, extraPaths ...string) error {
	if err := lynx.DefaultBindConfigFunc(f, c); err != nil {
		return err
	}

	for _, path := range extraPaths {
		if path == "" {
			continue
		}
		c.AddSearchPath(path)
	}

	c.SetEnvPrefix(EnvPrefix)
	c.AutomaticEnv()

	// 旧版通过 viper.SetEnvKeyReplacer 让任意点号键都能被 TORCHWOOD_* 覆盖；
	// lynx v1.0.0 的 ConfigSource 接口不再暴露该能力，这里改为对每个键
	// 显式 BindEnv 字面量环境变量名，语义不变。
	for _, key := range configKeys() {
		if err := c.BindEnv(key, envNameForKey(key)); err != nil {
			return err
		}
	}

	return nil
}

func NewBindConfigFunc(extraPaths ...string) lynx.BindConfigFunc {
	return func(f *pflag.FlagSet, c lynx.ConfigSource) error {
		return ConfigureViper(f, c, extraPaths...)
	}
}

// configKeys 返回 AppConfig 的所有叶子键路径（点号分隔），由 proto 生成的
// json tag 推导，例如 "storage.s3.access_key_id"。
func configKeys() []string {
	var keys []string
	collectKeys(reflect.TypeOf((*AppConfig)(nil)).Elem(), "", func(path string) {
		keys = append(keys, path)
	})
	return keys
}

// UnmarshalConfig 把 lynx.Config 解码到 AppConfig。lynx v1.0.0 的
// Config.Unmarshal 只按 mapstructure 字段名（大小写不敏感）匹配，无法识别
// access_key_id 这类 snake_case 键；这里按 proto json tag 逐叶子取值，
// 保证环境变量 / flag 覆盖与旧版 TagNameJSON 行为一致。
func UnmarshalConfig(c lynx.Config, out *AppConfig) error {
	root := make(map[string]any)
	collectLeaves(reflect.TypeOf(out).Elem(), "", c, root)

	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           out,
		TagName:          "json",
		WeaklyTypedInput: true,
		DecodeHook:       mapstructure.StringToSliceHookFunc(","),
	})
	if err != nil {
		return err
	}
	return decoder.Decode(root)
}

// collectKeys 递归收集 message 结构的所有叶子 json 路径。
func collectKeys(t reflect.Type, prefix string, emit func(path string)) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := jsonTagName(f)
		if name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			collectKeys(ft, path, emit)
		} else {
			emit(path)
		}
	}
}

// collectLeaves 按 json tag 逐叶子从 Config 取值，组装为嵌套 map。
func collectLeaves(t reflect.Type, prefix string, c lynx.Config, out map[string]any) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := jsonTagName(f)
		if name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct:
			child, ok := out[name].(map[string]any)
			if !ok {
				child = make(map[string]any)
				out[name] = child
			}
			collectLeaves(ft, path, c, child)
		default:
			if v := c.Get(path); v != nil {
				out[name] = v
			}
		}
	}
}

// jsonTagName 返回 proto 生成的 json tag 中的字段名；无 tag 或 "-" 时返回空串。
func jsonTagName(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	return name
}
