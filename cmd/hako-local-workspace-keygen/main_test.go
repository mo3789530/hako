//go:build linux

package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateCreatesPrivateKeyBundleAndLocalhostCertificate(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "keys")
	if err := generate(directory, time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("key directory mode=%v err=%v", info.Mode(), err)
	}
	for _, name := range []string{"session-private.pem", "session-public.json", "session-hmac.key", "agent-private.pem", "agent-public.json", "gateway-cert.pem", "gateway-key.pem"} {
		fileInfo, err := os.Stat(filepath.Join(directory, name))
		if err != nil || fileInfo.Mode().Perm() != 0600 {
			t.Fatalf("generated file %s mode=%v err=%v", name, fileInfo.Mode(), err)
		}
	}
	for _, item := range []struct {
		privateFile string
		publicFile  string
		keyID       string
		encoding    *base64.Encoding
	}{
		{"session-private.pem", "session-public.json", "local_session", base64.RawStdEncoding},
		{"agent-private.pem", "agent-public.json", "local_agent", base64.RawURLEncoding},
	} {
		privatePEM, err := os.ReadFile(filepath.Join(directory, item.privateFile))
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(privatePEM)
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		privateKey, ok := parsed.(ed25519.PrivateKey)
		if err != nil || !ok {
			t.Fatalf("generated private key %s is invalid: %v", item.privateFile, err)
		}
		encoded, err := os.ReadFile(filepath.Join(directory, item.publicFile))
		if err != nil {
			t.Fatal(err)
		}
		var publicKeys map[string]string
		if err := json.Unmarshal(encoded, &publicKeys); err != nil || len(publicKeys) != 1 {
			t.Fatalf("generated public key file %s is invalid: %v", item.publicFile, err)
		}
		publicKey, err := item.encoding.DecodeString(publicKeys[item.keyID])
		if err != nil || string(publicKey) != string(privateKey.Public().(ed25519.PublicKey)) {
			t.Fatalf("generated public key %s does not match private key", item.publicFile)
		}
	}
	hmac, err := os.ReadFile(filepath.Join(directory, "session-hmac.key"))
	if err != nil || len(hmac) != 32 {
		t.Fatalf("HMAC key length=%d err=%v", len(hmac), err)
	}
	certificatePEM, err := os.ReadFile(filepath.Join(directory, "gateway-cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificatePEM)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || certificate.Subject.CommonName != "localhost" || len(certificate.IPAddresses) != 2 {
		t.Fatalf("generated localhost certificate=%+v err=%v", certificate, err)
	}
}

func TestGenerateRejectsExistingDirectoryAndInvalidPath(t *testing.T) {
	directory := t.TempDir()
	if err := generate(directory, time.Now()); err == nil {
		t.Fatal("existing output directory must not be overwritten")
	}
	if err := generate("relative-keys", time.Now()); err == nil {
		t.Fatal("relative output directory must be rejected")
	}
}
