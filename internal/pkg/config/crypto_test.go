package config

import (
	"strings"
	"testing"
)

func TestEncryptionSecret(t *testing.T) {
	explicit := &AppConfig{Security: &Security{
		EncryptionKey: "  dedicated-key-32-bytes-aaaaaaaaa  ",
		Jwt:           &Security_Jwt{Secret: "jwt-secret"},
	}}
	got, fallback := EncryptionSecret(explicit)
	if fallback || got != "dedicated-key-32-bytes-aaaaaaaaa" {
		t.Fatalf("显式配置必须优先且 TrimSpace，got %q fallback=%v", got, fallback)
	}

	fallbackCfg := &AppConfig{Security: &Security{Jwt: &Security_Jwt{Secret: "jwt-secret"}}}
	got, fallback = EncryptionSecret(fallbackCfg)
	if !fallback || got != "jwt-secret" {
		t.Fatalf("未配置时回退 jwt.secret，got %q fallback=%v", got, fallback)
	}

	if got, fallback = EncryptionSecret(nil); got != "" || fallback {
		t.Fatalf("nil 配置返回空串且不标记回退，got %q fallback=%v", got, fallback)
	}

	// 空白串视为未配置（只填空白不能绕过回退）。
	blank := &AppConfig{Security: &Security{EncryptionKey: strings.Repeat(" ", 4), Jwt: &Security_Jwt{Secret: "jwt-secret"}}}
	if _, fallback = EncryptionSecret(blank); !fallback {
		t.Fatal("空白 encryption_key 必须视为未配置")
	}
}
