package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

type CredentialCipher interface {
	Encrypt([]byte) ([]byte, []byte, int, error)
	Decrypt([]byte, []byte, int) ([]byte, error)
}
type AESGCM struct {
	key     []byte
	version int
}

func NewAESGCM(value string, version int) (*AESGCM, error) {
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != 32 {
		if len(value) != 32 {
			return nil, fmt.Errorf("CREDENTIAL_ENCRYPTION_KEY must be 32 raw bytes or base64-encoded 32 bytes")
		}
		key = []byte(value)
	}
	if version <= 0 {
		version = 1
	}
	return &AESGCM{key: key, version: version}, nil
}
func (c *AESGCM) Encrypt(plaintext []byte) ([]byte, []byte, int, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, nil, 0, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, 0, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, 0, err
	}
	return gcm.Seal(nil, nonce, plaintext, nil), nonce, c.version, nil
}
func (c *AESGCM) Decrypt(ciphertext, nonce []byte, version int) ([]byte, error) {
	if version != c.version {
		return nil, fmt.Errorf("unsupported credential key version %d", version)
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}
