package security

import (
	"bytes"
	"testing"
)

func TestCredentialRoundTripAndTamper(t *testing.T) {
	c, err := NewAESGCM("01234567890123456789012345678901", 1)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("secret-token")
	ct, nonce, v, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct, plain) || len(nonce) == 0 {
		t.Fatal("credential not encrypted")
	}
	got, err := c.Decrypt(ct, nonce, v)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal(err)
	}
	ct[0] ^= 1
	if _, err = c.Decrypt(ct, nonce, v); err == nil {
		t.Fatal("tamper should fail")
	}
}
func TestCredentialNonceDiffers(t *testing.T) {
	c, _ := NewAESGCM("01234567890123456789012345678901", 1)
	a, na, _, _ := c.Encrypt([]byte("x"))
	b, nb, _, _ := c.Encrypt([]byte("x"))
	if bytes.Equal(a, b) || bytes.Equal(na, nb) {
		t.Fatal("nonce/ciphertext reused")
	}
}
