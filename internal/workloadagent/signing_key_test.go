package workloadagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTicketSigningKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	got, err := LoadTicketSigningKey(strings.NewReader(string(pemBytes)))
	if err != nil || !got.Equal(privateKey) {
		t.Fatalf("LoadTicketSigningKey() = key match %t, err %v", got.Equal(privateKey), err)
	}
	got[0] ^= 0xff
	if privateKey[0] == got[0] {
		t.Fatal("loader returned aliased private-key storage")
	}
}

func TestLoadTicketSigningKeyRejectsInvalidAndOversizedInput(t *testing.T) {
	_, rsaKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	wrongType := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded})
	valid, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	validPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: valid})
	for name, input := range map[string]string{
		"nil reader":      "",
		"wrong PEM label": string(wrongType),
		"trailing data":   string(validPEM) + "trailing",
		"oversized":       strings.Repeat("x", maxSigningKeyFileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadTicketSigningKey(strings.NewReader(input)); err == nil {
				t.Fatal("invalid signing-key input was accepted")
			}
		})
	}
	if _, err := LoadTicketSigningKey(nil); err == nil {
		t.Fatal("nil reader was accepted")
	}
}

func TestLoadTicketSigningKeyFileRequiresPrivateRegularFile(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	content := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	path := filepath.Join(t.TempDir(), "ticket-key.pem")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadTicketSigningKeyFile(path)
	if err != nil || !loaded.Equal(privateKey) {
		t.Fatalf("private file load = match %t, err %v", loaded.Equal(privateKey), err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTicketSigningKeyFile(path); err == nil {
		t.Fatal("group/world-readable signing key file was accepted")
	}
	if _, err := LoadTicketSigningKeyFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing signing key file was accepted")
	}
}
