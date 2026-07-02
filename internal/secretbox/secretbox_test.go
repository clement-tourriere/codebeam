package secretbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mustCipher(t *testing.T, key string) *Cipher {
	t.Helper()
	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher(%q): %v", key, err)
	}
	return c
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := mustCipher(t, "master-key")
	const token = "ghp_supersecrettoken123"

	enc, err := c.Encrypt(token)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(enc, Marker) {
		t.Fatalf("ciphertext missing marker: %q", enc)
	}
	if strings.Contains(enc, token) {
		t.Fatalf("ciphertext leaks plaintext: %q", enc)
	}
	if !IsEncrypted(enc) {
		t.Fatalf("IsEncrypted(%q) = false", enc)
	}

	got, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != token {
		t.Fatalf("round-trip = %q, want %q", got, token)
	}
}

func TestEncryptEmptyHasNoMarker(t *testing.T) {
	c := mustCipher(t, "master-key")
	enc, err := c.Encrypt("")
	if err != nil {
		t.Fatalf("Encrypt(\"\"): %v", err)
	}
	if enc != "" {
		t.Fatalf("Encrypt(\"\") = %q, want empty (preserves the WHERE col != '' sentinel)", enc)
	}
	if IsEncrypted(enc) {
		t.Fatalf("empty value must not be marked as encrypted")
	}
}

func TestDecryptEmpty(t *testing.T) {
	c := mustCipher(t, "master-key")
	got, err := c.Decrypt("")
	if err != nil {
		t.Fatalf("Decrypt(\"\"): %v", err)
	}
	if got != "" {
		t.Fatalf("Decrypt(\"\") = %q, want empty", got)
	}
}

func TestDecryptLegacyPlaintextPassthrough(t *testing.T) {
	c := mustCipher(t, "master-key")
	const legacy = "ghp_a_plaintext_token_from_before_encryption"
	got, err := c.Decrypt(legacy)
	if err != nil {
		t.Fatalf("Decrypt(legacy): %v", err)
	}
	if got != legacy {
		t.Fatalf("legacy passthrough = %q, want %q", got, legacy)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	enc, err := mustCipher(t, "key-a").Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := mustCipher(t, "key-b").Decrypt(enc)
	if err == nil {
		t.Fatalf("Decrypt with wrong key succeeded (%q); want error, never a plaintext fallback", got)
	}
	if got != "" {
		t.Fatalf("failed Decrypt returned non-empty %q", got)
	}
}

func TestDecryptTamperedFails(t *testing.T) {
	c := mustCipher(t, "master-key")
	enc, err := c.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip a byte in the base64 payload (after the marker).
	body := []byte(enc[len(Marker):])
	if body[0] == 'A' {
		body[0] = 'B'
	} else {
		body[0] = 'A'
	}
	tampered := Marker + string(body)
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatalf("Decrypt of tampered ciphertext succeeded; want auth failure")
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	c := mustCipher(t, "master-key")
	a, err := c.Encrypt("same-input")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := c.Encrypt("same-input")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if a == b {
		t.Fatalf("two encryptions of the same input are identical; nonce not random")
	}
	for _, enc := range []string{a, b} {
		got, err := c.Decrypt(enc)
		if err != nil || got != "same-input" {
			t.Fatalf("Decrypt(%q) = %q, %v", enc, got, err)
		}
	}
}

func TestNewCipherRejectsEmptyKey(t *testing.T) {
	if _, err := NewCipher(""); err == nil {
		t.Fatalf("NewCipher(\"\") succeeded; want error")
	}
}

func TestResolveKeyInlineWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encryption.key")
	key, generated, err := ResolveKey("inline-secret", path)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if key != "inline-secret" || generated {
		t.Fatalf("ResolveKey = (%q, %v), want (inline-secret, false)", key, generated)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("key file should not be created when an inline key is set")
	}
}

func TestResolveKeyGeneratesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "encryption.key")

	key1, generated, err := ResolveKey("", path)
	if err != nil {
		t.Fatalf("ResolveKey (first): %v", err)
	}
	if !generated || key1 == "" {
		t.Fatalf("first ResolveKey = (%q, generated=%v), want a generated key", key1, generated)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("key file perms = %o, want 0600", perm)
		}
	}

	key2, generated, err := ResolveKey("", path)
	if err != nil {
		t.Fatalf("ResolveKey (second): %v", err)
	}
	if generated {
		t.Fatalf("second ResolveKey reported generated=true; want reuse")
	}
	if key2 != key1 {
		t.Fatalf("second ResolveKey = %q, want reuse of %q", key2, key1)
	}
}

func TestResolveKeyErrorsWhenNothingConfigured(t *testing.T) {
	if _, _, err := ResolveKey("", ""); err == nil {
		t.Fatalf("ResolveKey(\"\", \"\") succeeded; want error")
	}
}
