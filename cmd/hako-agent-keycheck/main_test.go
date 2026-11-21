package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunValidatesPublicKeyFile(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]string{
		"resource-plane-v1": base64.RawURLEncoding.EncodeToString(publicKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "trusted-keys.json")
	if err := os.WriteFile(keyFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if code := run([]string{"-file", keyFile}, &stderr); code != 0 {
		t.Fatalf("run() = %d, stderr=%q; want success", code, stderr.String())
	}
}

func TestRunRejectsInvalidKeyAndArgumentsWithoutLeakingInput(t *testing.T) {
	invalidFile := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalidFile, []byte(`{"private":"PRIVATE KEY material"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "private key material", args: []string{"-file", invalidFile}, want: 1},
		{name: "missing file", args: []string{"-file", filepath.Join(t.TempDir(), "missing.json")}, want: 1},
		{name: "missing file flag", args: []string{"unexpected"}, want: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(test.args, &stderr); code != test.want {
				t.Fatalf("run() = %d, want %d; stderr=%q", code, test.want, stderr.String())
			}
			if strings.Contains(stderr.String(), "material") {
				t.Fatalf("stderr contains key material: %q", stderr.String())
			}
		})
	}
}
