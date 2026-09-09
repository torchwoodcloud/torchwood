package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
)

const prefix = "enc:v1:"

// deriveKey 收敛至 jwtparser.DeriveKey（HMAC-SHA256 purpose 派生，P3-6），
// 与 OTP 等其它 KDF 共用单一入口，便于审计与轮换。
func deriveKey(secret string) []byte {
	return jwtparser.DeriveKey(secret, jwtparser.PurposeSecretBox)
}

// Encrypt seals plaintext with AES-256-GCM using secret as key material.
func Encrypt(plaintext, secret string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if secret == "" {
		return "", errors.New("encryption secret is required")
	}
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt opens a value produced by Encrypt. Legacy plaintext values pass through.
func Decrypt(stored, secret string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, prefix) {
		return stored, nil
	}
	if secret == "" {
		return "", errors.New("encryption secret is required")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err == nil {
		return string(plaintext), nil
	}
	// 兼容旧 KDF（sha256 单轮）：P3-6 迁移期存量数据仍可解密，解密后下次写会用新 KDF 重新加密。
	legacyKey := sha256.Sum256([]byte("torchwood-secretbox:" + secret))
	if block, lerr := aes.NewCipher(legacyKey[:]); lerr == nil {
		if gcm2, lerr2 := cipher.NewGCM(block); lerr2 == nil && len(raw) >= gcm2.NonceSize() {
			if pt, lerr3 := gcm2.Open(nil, raw[:gcm2.NonceSize()], raw[gcm2.NonceSize():], nil); lerr3 == nil {
				return string(pt), nil
			}
		}
	}
	return "", err
}
