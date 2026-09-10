package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// decodeJSON 以 UseNumber 解码，避免 64 位整型经 float64 丢精度。
func decodeJSON(s string, v any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	return dec.Decode(v)
}

// jsonStringList 解析 JSON 字符串数组 flag（如 --permissions '["a","b"]'、
// --queries '["equal(\"status\",\"active\")"]'），空串返回 nil。
func jsonStringList(s, flagName string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := decodeJSON(s, &out); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %v", flagName, err)
	}
	return out, nil
}

// jsonStringMap 解析 JSON 对象 flag（map<string,string>，如 --metadata、--vars）。
func jsonStringMap(s, flagName string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]string
	if err := decodeJSON(s, &out); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %v", flagName, err)
	}
	return out, nil
}

// jsonInt64Map 解析 JSON 对象 flag（map<string,int64>，如 --increment '{"views":1}'）；
// 用 json.Number 保持精度，>2^53 的整数不丢位。
func jsonInt64Map(s, flagName string) (map[string]json.Number, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]json.Number
	if err := decodeJSON(s, &out); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %v", flagName, err)
	}
	return out, nil
}

// jsonObject 解析 JSON 对象 flag 为 map（UseNumber）。
func jsonObject(s, flagName string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]any
	if err := decodeJSON(s, &out); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %v", flagName, err)
	}
	return out, nil
}

// mergeJSON 把 --data JSON 覆盖合并进请求 map（--data 与 flag 冲突时以 --data 为准）。
func mergeJSON(m map[string]any, data string) error {
	if data == "" {
		return nil
	}
	var dm map[string]any
	if err := decodeJSON(data, &dm); err != nil {
		return fmt.Errorf("failed to parse --data: %v", err)
	}
	for k, v := range dm {
		m[k] = v
	}
	return nil
}

// setChanged 在旗标显式设置时写入请求 map（proto3 optional presence 用键存
// 在性表达；presence 经动词捕获的 FlagSet 判断，见 verb.changed）。
func setChanged(v *verb, flag string, m map[string]any, key string, val any) {
	if v.changed(flag) {
		m[key] = val
	}
}

// listJSON 构造分页请求 map（仅放非零键）。
func listJSON(pageSize int, pageToken string) map[string]any {
	m := map[string]any{}
	if pageSize > 0 {
		m["pageSize"] = pageSize
	}
	if pageToken != "" {
		m["pageToken"] = pageToken
	}
	return m
}
