package workloadartifactaccess

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTicketIssueVerifyAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{0x5a}, 32)
	claims := Claims{UserID: "usr_123", TenantID: "tenant_123", WorkloadRunID: "run_123", ArtifactID: "artifact_123", ResourcePlane: "rp_tokyo_1"}
	token, issued, err := Issue(key, claims, now, bytes.NewReader(bytes.Repeat([]byte{0x01}, 24)))
	if err != nil {
		t.Fatal(err)
	}
	if issued.Audience != Audience || issued.Version != 1 || issued.ExpiresAt-issued.IssuedAt != int64(TicketMaxTTL/time.Second) {
		t.Fatalf("issued claims = %+v", issued)
	}
	verified, err := Verify(token, key, now.Add(time.Second))
	if err != nil || verified != issued {
		t.Fatalf("Verify() = %+v, %v", verified, err)
	}
	if _, err := Verify(token, key, now.Add(TicketMaxTTL)); err == nil {
		t.Fatal("expired artifact ticket was accepted")
	}
}

func TestTicketRejectsTamperingAndMalformedClaims(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{0x5a}, 32)
	base := Claims{UserID: "usr_123", TenantID: "tenant_123", WorkloadRunID: "run_123", ArtifactID: "artifact_123", ResourcePlane: "rp_tokyo_1"}
	token, claims, err := Issue(key, base, now, bytes.NewReader(bytes.Repeat([]byte{0x01}, 24)))
	if err != nil {
		t.Fatal(err)
	}
	mutated := claims
	mutated.ArtifactID = "artifact_other"
	payload, _ := json.Marshal(mutated)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	for _, test := range []struct {
		name  string
		token string
		key   []byte
	}{
		{name: "altered artifact", token: encoded + "." + strings.Split(token, ".")[1], key: key},
		{name: "wrong plane key", token: token, key: bytes.Repeat([]byte{0x6b}, 32)},
		{name: "extra component", token: token + ".extra", key: key},
		{name: "oversized", token: strings.Repeat("a", maxTicketSize+1), key: key},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Verify(test.token, test.key, now); err == nil {
				t.Fatal("Verify() accepted invalid ticket")
			}
		})
	}
}

func TestTicketIssueRejectsInvalidInput(t *testing.T) {
	now := time.Now().UTC()
	key := bytes.Repeat([]byte{0x5a}, 32)
	base := Claims{UserID: "usr_123", TenantID: "tenant_123", WorkloadRunID: "run_123", ArtifactID: "artifact_123", ResourcePlane: "rp_tokyo_1"}
	tests := []struct {
		name   string
		key    []byte
		claims Claims
		now    time.Time
	}{
		{name: "short key", key: []byte("short"), claims: base, now: now},
		{name: "empty user", key: key, claims: Claims{TenantID: base.TenantID, WorkloadRunID: base.WorkloadRunID, ArtifactID: base.ArtifactID, ResourcePlane: base.ResourcePlane}, now: now},
		{name: "path-like artifact", key: key, claims: func() Claims { c := base; c.ArtifactID = "../other"; return c }(), now: now},
		{name: "invalid plane", key: key, claims: func() Claims { c := base; c.ResourcePlane = "rp/other"; return c }(), now: now},
		{name: "zero time", key: key, claims: base},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := Issue(test.key, test.claims, test.now, nil); err == nil {
				t.Fatal("Issue() accepted invalid input")
			}
		})
	}
}
