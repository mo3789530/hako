package workspaceagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const (
	serviceCapabilityAudience = "hako-workspace-service"
	serviceCapabilityMaxTTL   = 2 * time.Minute
	serviceCapabilityMaxBytes = 8192
)

var serviceCapabilityIDPattern = regexp.MustCompile(`^[a-f0-9]{48}$`)

type ServiceCapability struct {
	Version              int                    `json:"version"`
	Audience             string                 `json:"audience"`
	ID                   string                 `json:"id"`
	TenantID             domain.TenantID        `json:"tenant_id"`
	WorkspaceID          domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlane        domain.ResourcePlaneID `json:"resource_plane_id"`
	MicroVMID            string                 `json:"microvm_id"`
	ServiceID            domain.ServiceID       `json:"service_id"`
	Revision             int64                  `json:"revision"`
	CredentialGeneration int64                  `json:"credential_generation"`
	DesiredState         string                 `json:"desired_state"`
	CatalogVersion       string                 `json:"catalog_version"`
	ConfigSHA256         string                 `json:"config_sha256"`
	IssuedAt             int64                  `json:"issued_at"`
	ExpiresAt            int64                  `json:"expires_at"`
}

type ServiceCapabilityIssuer struct {
	KeyID      string
	PrivateKey ed25519.PrivateKey
	TTL        time.Duration
	Now        func() time.Time
	Random     io.Reader
}

func (i *ServiceCapabilityIssuer) Issue(command domain.WorkspaceServiceCommand, microVMID string) (string, ServiceCapability, error) {
	if i == nil || !agentIdentityPattern.MatchString(i.KeyID) || len(i.PrivateKey) != ed25519.PrivateKeySize ||
		!validServiceCommandIdentity(command, microVMID) {
		return "", ServiceCapability{}, ErrServiceCommand
	}
	if _, err := validatedServiceTemplate(command); err != nil {
		return "", ServiceCapability{}, err
	}
	ttl := i.TTL
	if ttl <= 0 {
		ttl = serviceCapabilityMaxTTL
	}
	if ttl < time.Second || ttl > serviceCapabilityMaxTTL {
		return "", ServiceCapability{}, ErrServiceCommand
	}
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	random := i.Random
	if random == nil {
		random = rand.Reader
	}
	var rawID [24]byte
	if _, err := io.ReadFull(random, rawID[:]); err != nil {
		return "", ServiceCapability{}, ErrServiceCommand
	}
	issued := now().UTC()
	configDigest := sha256.Sum256(command.Config)
	claims := ServiceCapability{Version: 1, Audience: serviceCapabilityAudience, ID: hex.EncodeToString(rawID[:]),
		TenantID: command.TenantID, WorkspaceID: command.WorkspaceID, ResourcePlane: command.ResourcePlaneID,
		MicroVMID: microVMID, ServiceID: command.ServiceID, Revision: command.Revision,
		CredentialGeneration: serviceCommandCredentialGeneration(command),
		DesiredState:         command.DesiredState, CatalogVersion: command.CatalogVersion,
		ConfigSHA256: hex.EncodeToString(configDigest[:]), IssuedAt: issued.Unix(), ExpiresAt: issued.Add(ttl).Unix()}
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > serviceCapabilityMaxBytes {
		return "", ServiceCapability{}, ErrServiceCommand
	}
	message := i.KeyID + "." + base64.RawURLEncoding.EncodeToString(payload)
	token := message + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(i.PrivateKey, []byte(message)))
	return token, claims, nil
}

func VerifyServiceCapability(token string, keys map[string]ed25519.PublicKey, expected ServiceCapability, command domain.WorkspaceServiceCommand, now time.Time) (ServiceCapability, error) {
	if expected.CredentialGeneration == 0 {
		expected.CredentialGeneration = 1
	}
	if len(token) == 0 || len(token) > serviceCapabilityMaxBytes || len(keys) == 0 ||
		!validServiceCommandIdentity(command, expected.MicroVMID) || !validServiceCapabilityBinding(expected) {
		return ServiceCapability{}, ErrServiceCommand
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !agentIdentityPattern.MatchString(parts[0]) {
		return ServiceCapability{}, ErrServiceCommand
	}
	key := keys[parts[0]]
	if len(key) != ed25519.PublicKeySize {
		return ServiceCapability{}, ErrServiceCommand
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > serviceCapabilityMaxBytes {
		return ServiceCapability{}, ErrServiceCommand
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return ServiceCapability{}, ErrServiceCommand
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var claims ServiceCapability
	if decoder.Decode(&claims) != nil || decoder.Decode(new(any)) != io.EOF ||
		claims.Version != 1 || claims.Audience != serviceCapabilityAudience || !serviceCapabilityIDPattern.MatchString(claims.ID) ||
		claims.TenantID != expected.TenantID || claims.WorkspaceID != expected.WorkspaceID ||
		claims.ResourcePlane != expected.ResourcePlane || claims.MicroVMID != expected.MicroVMID ||
		claims.ServiceID != expected.ServiceID || claims.Revision != expected.Revision || claims.CredentialGeneration != expected.CredentialGeneration || claims.DesiredState != expected.DesiredState ||
		claims.CatalogVersion != expected.CatalogVersion || claims.TenantID != command.TenantID || claims.WorkspaceID != command.WorkspaceID ||
		claims.ResourcePlane != command.ResourcePlaneID || claims.ServiceID != command.ServiceID || claims.Revision != command.Revision ||
		claims.CredentialGeneration != serviceCommandCredentialGeneration(command) ||
		claims.DesiredState != command.DesiredState || claims.CatalogVersion != command.CatalogVersion || claims.ConfigSHA256 != serviceConfigDigest(command.Config) {
		return ServiceCapability{}, ErrServiceCommand
	}
	issued, expires := time.Unix(claims.IssuedAt, 0).UTC(), time.Unix(claims.ExpiresAt, 0).UTC()
	if claims.IssuedAt <= 0 || !now.Before(expires) || expires.Sub(issued) <= 0 || expires.Sub(issued) > serviceCapabilityMaxTTL || issued.After(now.Add(5*time.Second)) {
		return ServiceCapability{}, ErrServiceCommand
	}
	return claims, nil
}

func validServiceCommandIdentity(command domain.WorkspaceServiceCommand, microVMID string) bool {
	return command.SchemaVersion == 1 && agentIdentityPattern.MatchString(string(command.TenantID)) &&
		agentIdentityPattern.MatchString(string(command.WorkspaceID)) && agentIdentityPattern.MatchString(string(command.ResourcePlaneID)) &&
		agentIdentityPattern.MatchString(microVMID) && agentIdentityPattern.MatchString(string(command.ServiceID)) &&
		command.Revision > 0 && command.CredentialGeneration >= 0 && (command.DesiredState == "running" || command.DesiredState == "stopped" || command.DesiredState == "deleted") &&
		command.CatalogVersion != "" && len(command.Config) > 0
}

func serviceCommandCredentialGeneration(command domain.WorkspaceServiceCommand) int64 {
	if command.CredentialGeneration == 0 {
		return 1
	}
	return command.CredentialGeneration
}

func validServiceCapabilityBinding(capability ServiceCapability) bool {
	return agentIdentityPattern.MatchString(string(capability.TenantID)) &&
		agentIdentityPattern.MatchString(string(capability.WorkspaceID)) &&
		agentIdentityPattern.MatchString(string(capability.ResourcePlane)) &&
		agentIdentityPattern.MatchString(capability.MicroVMID) &&
		agentIdentityPattern.MatchString(string(capability.ServiceID)) && capability.Revision > 0 && capability.CredentialGeneration > 0
}

func serviceConfigDigest(config []byte) string {
	digest := sha256.Sum256(config)
	return hex.EncodeToString(digest[:])
}

type ServiceCapabilityReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (g *ServiceCapabilityReplayGuard) Consume(capability ServiceCapability, now time.Time) error {
	if g == nil || !serviceCapabilityIDPattern.MatchString(capability.ID) || !now.Before(time.Unix(capability.ExpiresAt, 0)) {
		return ErrServiceCommand
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = make(map[string]time.Time)
	}
	for id, expiry := range g.seen {
		if !now.Before(expiry) {
			delete(g.seen, id)
		}
	}
	if _, exists := g.seen[capability.ID]; exists || len(g.seen) >= maxAgentReplayTickets {
		return errors.New("Workspace Service capability was already consumed")
	}
	g.seen[capability.ID] = time.Unix(capability.ExpiresAt, 0)
	return nil
}
