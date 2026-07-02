// Package secretbox provides authenticated symmetric encryption (AES-256-GCM)
// for the few secrets Codebeam must store reversibly at rest — currently the
// upstream code-host access tokens in the identities table, which the server
// decrypts and replays to GitHub/GitLab to clone private repositories. (Secrets
// Codebeam only ever verifies — PATs, OAuth client secrets — are SHA-256 hashed
// elsewhere and never come through here.)
//
// Encrypted values are self-identifying via a version marker, so a stored value
// can always be told apart from a legacy plaintext token that predates
// encryption, and the format can evolve (enc:v2:…) without ambiguity.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Marker prefixes every value produced by Encrypt. base64.StdEncoding's alphabet
// contains no colon, so a value carrying this prefix is unambiguously ciphertext
// and never a legacy plaintext token. Bump to enc:v2: if the scheme changes.
const Marker = "enc:v1:"

// Cipher encrypts and decrypts short secrets with AES-256-GCM under a key derived
// from a master secret. Its methods are safe for concurrent use.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher derives a 32-byte AES-256 key as SHA-256(masterKey) and returns a
// Cipher. masterKey may be any non-empty string; callers supply high-entropy
// material (see ResolveKey). The derivation is deterministic on purpose: the same
// key must decrypt across restarts, so there is no random salt to store.
func NewCipher(masterKey string) (*Cipher, error) {
	if masterKey == "" {
		return nil, errors.New("secretbox: master key is required")
	}
	key := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("secretbox: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// MustNewCipher is like NewCipher but panics on an invalid key. Handy for
// package-level initialization and tests where the key is known-good.
func MustNewCipher(masterKey string) *Cipher {
	c, err := NewCipher(masterKey)
	if err != nil {
		panic(err)
	}
	return c
}

// IsEncrypted reports whether v was produced by Encrypt, as opposed to a legacy
// plaintext value that predates encryption.
func IsEncrypted(v string) bool {
	return strings.HasPrefix(v, Marker)
}

// Encrypt returns Marker + base64(nonce || ciphertext || tag) with a fresh random
// nonce. An empty input returns an empty string with no marker, so an emptiness
// check on the stored value keeps meaning "no secret stored".
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: read nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return Marker + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. An empty input returns empty. A value without the
// marker is treated as legacy plaintext and returned unchanged, so reads keep
// working before and during the one-time migration. A marked value that fails to
// decode or authenticate (wrong key or tampering) returns an error and never
// falls back to the raw bytes. Error messages never contain the secret.
func (c *Cipher) Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !IsEncrypted(stored) {
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(stored[len(Marker):])
	if err != nil {
		return "", fmt.Errorf("secretbox: decode ciphertext: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("secretbox: ciphertext too short")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("secretbox: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// GenerateKey returns fresh, high-entropy key material: 32 random bytes from
// crypto/rand, base64url-encoded.
func GenerateKey() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("secretbox: generate key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// ResolveKey returns the master key to use, in precedence order:
//
//  1. inlineKey, when non-empty (e.g. from CODEBEAM_ENCRYPTION_KEY).
//  2. the contents of keyFilePath — generating a fresh key and persisting it
//     there (file 0600, parent dir 0700) when the file does not yet exist, so a
//     normal single-host deployment gets encryption automatically with no setup.
//
// generated is true only when a new key file was just created, letting the caller
// warn that any pre-existing ciphertext may no longer be decryptable. When both
// inlineKey and keyFilePath are empty it returns an error rather than silently
// running without encryption. Creation uses O_EXCL so two processes racing on
// first run converge on a single key instead of clobbering each other.
func ResolveKey(inlineKey, keyFilePath string) (key string, generated bool, err error) {
	if inlineKey != "" {
		return inlineKey, false, nil
	}
	if keyFilePath == "" {
		return "", false, errors.New("secretbox: no encryption key configured: set CODEBEAM_ENCRYPTION_KEY or CODEBEAM_ENCRYPTION_KEY_FILE")
	}

	if k, err := readKeyFile(keyFilePath); err == nil {
		return k, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}

	// File absent: generate and claim it exclusively.
	k, err := GenerateKey()
	if err != nil {
		return "", false, err
	}
	created, err := createKeyFile(keyFilePath, k)
	if err != nil {
		return "", false, err
	}
	if created {
		return k, true, nil
	}
	// Lost a first-run race with another process; adopt the winner's key.
	k, err = readKeyFile(keyFilePath)
	if err != nil {
		return "", false, err
	}
	return k, false, nil
}

// readKeyFile reads and trims a key file. A missing file surfaces as os.ErrNotExist
// so callers can distinguish "not yet created" from a real read error.
func readKeyFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err // includes os.ErrNotExist, checked by the caller
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("secretbox: encryption key file %s is empty", path)
	}
	return key, nil
}

// createKeyFile writes key to path with 0600, creating parent dirs (0700). It
// uses O_EXCL so a concurrent creator is detected: created=false, err=nil means
// the file already existed and the caller should read it instead.
func createKeyFile(path, key string) (created bool, err error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("secretbox: create key dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("secretbox: create key file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(key + "\n"); err != nil {
		return false, fmt.Errorf("secretbox: write key file: %w", err)
	}
	return true, nil
}
