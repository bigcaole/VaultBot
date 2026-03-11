package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
)

// DeriveKey 使用 Argon2id 从 MASTER_KEY + SECRET_PEPPER 派生 AES-256-GCM 密钥。
func DeriveKey(masterKeyRaw string, pepper string) ([]byte, error) {
	masterKeyRaw = strings.TrimSpace(masterKeyRaw)
	pepper = strings.TrimSpace(pepper)
	if masterKeyRaw == "" {
		return nil, errors.New("MASTER_KEY is required")
	}
	if pepper == "" {
		return nil, errors.New("SECRET_PEPPER is required")
	}
	masterKey, err := decodeMasterKey(masterKeyRaw)
	if err != nil {
		return nil, err
	}
	pepperBytes := []byte(pepper)
	combined := make([]byte, 0, len(masterKey)+1+len(pepperBytes))
	combined = append(combined, masterKey...)
	combined = append(combined, ':')
	combined = append(combined, pepperBytes...)
	salt := sha256.Sum256(append([]byte("vaultbot:"), pepperBytes...))
	derived := argon2.IDKey(combined, salt[:], argonTime, argonMemory, argonThreads, argonKeyLen)
	zeroize(masterKey)
	zeroize(pepperBytes)
	zeroize(combined)
	return derived, nil
}

// Encrypt 使用 AES-256-GCM 加密明文，返回密文与随机 Nonce（均为 base64）。
func Encrypt(plaintext string, masterKey []byte) (string, string, error) {
	data := []byte(plaintext)
	ciphertext, nonce, err := EncryptBytes(data, masterKey)
	return ciphertext, nonce, err
}

// EncryptBytes 使用 AES-256-GCM 加密明文（byte slice），返回密文与随机 Nonce（均为 base64）。
func EncryptBytes(plaintext []byte, masterKey []byte) (string, string, error) {
	if len(masterKey) != 32 {
		return "", "", errors.New("invalid master key length")
	}
	if len(plaintext) > 0 {
		defer zeroize(plaintext)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", err
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), base64.StdEncoding.EncodeToString(nonce), nil
}

// Decrypt 使用 AES-256-GCM 解密密文，ciphertext/nonce 为 base64 字符串。
func Decrypt(ciphertextB64 string, nonceB64 string, masterKey []byte) (string, error) {
	if len(masterKey) != 32 {
		return "", errors.New("invalid master key length")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		zeroize(ciphertext)
		return "", err
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		zeroize(ciphertext)
		zeroize(nonce)
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		zeroize(ciphertext)
		zeroize(nonce)
		return "", err
	}
	if len(nonce) != gcm.NonceSize() {
		zeroize(ciphertext)
		zeroize(nonce)
		return "", errors.New("invalid nonce size")
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	zeroize(ciphertext)
	zeroize(nonce)
	if err != nil {
		return "", err
	}
	out := string(plaintext)
	zeroize(plaintext)
	return out, nil
}

// DecryptWithFallback 尝试使用主密钥解密，失败时使用旧密钥。
func DecryptWithFallback(ciphertextB64 string, nonceB64 string, masterKey []byte, legacyKey []byte) (string, bool, error) {
	plaintext, err := Decrypt(ciphertextB64, nonceB64, masterKey)
	if err == nil {
		return plaintext, false, nil
	}
	if len(legacyKey) == 0 {
		return "", false, err
	}
	plaintext, err = Decrypt(ciphertextB64, nonceB64, legacyKey)
	if err != nil {
		return "", false, err
	}
	return plaintext, true, nil
}

// LoadRawKey 解析原始 MASTER_KEY（32 字节或 base64 形式），用于旧版本兼容解密。
func LoadRawKey(raw string) ([]byte, error) {
	return decodeMasterKey(raw)
}

func decodeMasterKey(raw string) ([]byte, error) {
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("MASTER_KEY must be 32 bytes or base64-encoded 32 bytes")
	}
	if len(decoded) != 32 {
		return nil, errors.New("MASTER_KEY must be 32 bytes after decoding")
	}
	return decoded, nil
}

func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
