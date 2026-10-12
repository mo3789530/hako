package workloadagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestLoadTrustedKeysAcceptsMultiplePublicKeysAndCopiesBytes(t *testing.T) {
	first, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]string{
		"current":  base64.RawURLEncoding.EncodeToString(first),
		"previous": base64.RawURLEncoding.EncodeToString(second),
	})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadTrustedKeys(strings.NewReader(string(encoded)))
	if err != nil || len(keys) != 2 {
		t.Fatalf("LoadTrustedKeys() keys=%d err=%v", len(keys), err)
	}
	first[0] ^= 0xff
	if keys["current"][0] == first[0] {
		t.Fatal("trusted key aliases the caller-owned input")
	}
}

func TestLoadTrustedKeysRejectsMalformedOversizedOrPrivateMaterial(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := json.Marshal(map[string]string{"agent-v1": base64.RawURLEncoding.EncodeToString(public)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "empty", data: ""},
		{name: "unknown field", data: `{"keys":{},"private_key":"must-not-be-accepted"}`},
		{name: "invalid key id", data: `{"not valid": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
		{name: "wrong key length", data: `{"agent-v1": "YQ"}`},
		{name: "trailing json", data: string(valid) + `{}`},
		{name: "oversized", data: `{"agent-v1":"` + strings.Repeat("a", maxTrustedKeysFileBytes) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if keys, err := LoadTrustedKeys(strings.NewReader(tc.data)); err == nil || keys != nil {
				t.Fatalf("LoadTrustedKeys() keys=%v err=%v; want fail-closed", keys, err)
			}
		})
	}
}
