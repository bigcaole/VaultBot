package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

// LoadMasterKey decodes the master key from env (raw 32 bytes or base64-encoded).
func LoadMasterKey(raw string) ([]byte, error) {
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

// Encrypt 使用 AES-256-GCM 加密明文，返回密文与随机 Nonce（均为 base64）。
func Encrypt(plaintext string, masterKey []byte) (string, string, error) {
	data := []byte(plaintext)
	ciphertext, nonce, err := EncryptBytes(data, masterKey)
	zeroize(data)
	return ciphertext, nonce, err
}

// EncryptBytes 使用 AES-256-GCM 加密明文（byte slice），返回密文与随机 Nonce（均为 base64）。
func EncryptBytes(plaintext []byte, masterKey []byte) (string, string, error) {
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
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(nonce) != gcm.NonceSize() {
		return "", errors.New("invalid nonce size")
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
