// Package workloadartifactaccess defines short-lived, Resource Plane-scoped
// capabilities for downloading one persisted Workload artifact.
package workloadartifactaccess

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	Audience      = "hako-workload-artifact"
	TicketMaxTTL  = 2 * time.Minute
	maxTicketSize = 8 << 10
)

var (
	ErrTicket       = errors.New("invalid Workload artifact ticket")
	identifier      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	artifactID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	resourcePlaneID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

// Claims authorize a single artifact identity. Storage locations are resolved
// by the Resource Plane through its authenticated Control Plane channel.
type Claims struct {
	Version       int    `json:"version"`
	Audience      string `json:"audience"`
	TicketID      string `json:"ticket_id"`
	UserID        string `json:"user_id"`
	TenantID      string `json:"tenant_id"`
	WorkloadRunID string `json:"workload_run_id"`
	ArtifactID    string `json:"artifact_id"`
	ResourcePlane string `json:"resource_plane_id"`
	IssuedAt      int64  `json:"issued_at"`
	ExpiresAt     int64  `json:"expires_at"`
}

func Issue(key []byte, claims Claims, now time.Time, random io.Reader) (string, Claims, error) {
	if len(key) < 32 || now.IsZero() || !validIdentity(claims) {
		return "", Claims{}, ErrTicket
	}
	if random == nil {
		random = rand.Reader
	}
	var nonce [24]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return "", Claims{}, ErrTicket
	}
	claims.Version = 1
	claims.Audience = Audience
	claims.TicketID = hex.EncodeToString(nonce[:])
	claims.IssuedAt = now.UTC().Unix()
	claims.ExpiresAt = now.UTC().Add(TicketMaxTTL).Unix()
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > maxTicketSize/2 {
		return "", Claims{}, ErrTicket
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(encoded))
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return token, claims, nil
}

func Verify(token string, key []byte, now time.Time) (Claims, error) {
	if len(key) < 32 || len(token) == 0 || len(token) > maxTicketSize || now.IsZero() {
		return Claims{}, ErrTicket
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return Claims{}, ErrTicket
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size {
		return Claims{}, ErrTicket
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return Claims{}, ErrTicket
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > maxTicketSize/2 {
		return Claims{}, ErrTicket
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var claims Claims
	if decoder.Decode(&claims) != nil || decoder.Decode(new(any)) != io.EOF || !validIdentity(claims) ||
		claims.Version != 1 || claims.Audience != Audience || !validTicketID(claims.TicketID) {
		return Claims{}, ErrTicket
	}
	issuedAt, expiresAt := time.Unix(claims.IssuedAt, 0), time.Unix(claims.ExpiresAt, 0)
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || expiresAt.Sub(issuedAt) > TicketMaxTTL ||
		issuedAt.After(now.Add(30*time.Second)) || !now.Before(expiresAt) || now.Sub(issuedAt) > TicketMaxTTL+30*time.Second {
		return Claims{}, ErrTicket
	}
	return claims, nil
}

func validIdentity(claims Claims) bool {
	return identifier.MatchString(claims.UserID) && identifier.MatchString(claims.TenantID) &&
		identifier.MatchString(claims.WorkloadRunID) && artifactID.MatchString(claims.ArtifactID) &&
		resourcePlaneID.MatchString(claims.ResourcePlane)
}

func validTicketID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 24 && len(id) == 48
}
