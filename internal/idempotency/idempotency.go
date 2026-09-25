// Package idempotency validates client keys and computes stable request hashes.
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const MaxKeyLength = 128

// ValidateKey accepts a visible ASCII token up to 128 bytes. Callers should
// generate a fresh UUIDv4 per logical request and reuse it only for retries.
func ValidateKey(key string) error {
	if len(key) == 0 || len(key) > MaxKeyLength {
		return errors.New("idempotency key must contain 1 to 128 bytes")
	}
	for _, b := range []byte(key) {
		if b < 0x21 || b > 0x7e {
			return errors.New("idempotency key must contain visible ASCII characters only")
		}
	}
	return nil
}

// Fingerprint returns a SHA-256 hex digest of a normalized request value.
// Callers should pass a request-specific struct with defaults already applied,
// rather than a transport object containing headers or generated timestamps.
func Fingerprint(normalizedRequest any) (string, error) {
	encoded, err := json.Marshal(normalizedRequest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
