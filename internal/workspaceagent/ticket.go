package workspaceagent

import (
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/mo3789530/hako/internal/workspacesession"
)

const (
	workspaceAgentAudience  = "hako-workspace-agent"
	workspaceAgentTicketTTL = 2 * time.Minute
	maxAgentTicketBytes     = 8192
	maxAgentReplayTickets   = 10000
)

var agentIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var agentTicketIDPattern = regexp.MustCompile(`^[a-f0-9]{48}$`)

var ErrAgentTicket = errors.New("invalid Workspace Agent ticket")

// AgentTicket is a Gateway-to-MicroVM capability, distinct from the browser
// Hako Session Ticket. It is pinned to one MicroVM, Workspace, Resource Plane,
// user, permission, and short expiry.
type AgentTicket struct {
	Version       int                         `json:"version"`
	Audience      string                      `json:"audience"`
	TicketID      string                      `json:"ticket_id"`
	UserID        domain.UserID               `json:"user_id"`
	TenantID      domain.TenantID             `json:"tenant_id"`
	WorkspaceID   domain.WorkspaceID          `json:"workspace_id"`
	ResourcePlane domain.ResourcePlaneID      `json:"resource_plane_id"`
	MicroVMID     string                      `json:"microvm_id"`
	Permission    workspacesession.Permission `json:"permission"`
	Port          uint16                      `json:"port,omitempty"`
	IssuedAt      int64                       `json:"issued_at"`
	ExpiresAt     int64                       `json:"expires_at"`
}

type AgentTicketIssuer struct {
	KeyID      string
	PrivateKey ed25519.PrivateKey
	TTL        time.Duration
	Now        func() time.Time
	Random     io.Reader
}

func (i *AgentTicketIssuer) Issue(input AgentTicket) (string, AgentTicket, error) {
	if i == nil || !agentIdentityPattern.MatchString(i.KeyID) || len(i.PrivateKey) != ed25519.PrivateKeySize ||
		!validAgentTicketIdentity(input) || !validAgentTarget(input) {
		return "", AgentTicket{}, ErrAgentTicket
	}
	ttl := i.TTL
	if ttl <= 0 {
		ttl = workspaceAgentTicketTTL
	}
	if ttl < time.Second || ttl > workspaceAgentTicketTTL {
		return "", AgentTicket{}, ErrAgentTicket
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
		return "", AgentTicket{}, ErrAgentTicket
	}
	issued := now().UTC()
	input.Version = 1
	input.Audience = workspaceAgentAudience
	input.TicketID = hex.EncodeToString(rawID[:])
	input.IssuedAt = issued.Unix()
	input.ExpiresAt = issued.Add(ttl).Unix()
	payload, err := json.Marshal(input)
	if err != nil || len(payload) > maxAgentTicketBytes {
		return "", AgentTicket{}, ErrAgentTicket
	}
	message := i.KeyID + "." + base64.RawURLEncoding.EncodeToString(payload)
	token := message + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(i.PrivateKey, []byte(message)))
	return token, input, nil
}

func VerifyAgentTicket(token string, keys map[string]ed25519.PublicKey, expected AgentTicket, now time.Time) (AgentTicket, error) {
	if len(token) == 0 || len(token) > maxAgentTicketBytes || len(keys) == 0 ||
		!validAgentTicketBinding(expected) || !validAgentTarget(expected) {
		return AgentTicket{}, ErrAgentTicket
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !agentIdentityPattern.MatchString(parts[0]) {
		return AgentTicket{}, ErrAgentTicket
	}
	key := keys[parts[0]]
	if len(key) != ed25519.PublicKeySize {
		return AgentTicket{}, ErrAgentTicket
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > maxAgentTicketBytes {
		return AgentTicket{}, ErrAgentTicket
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return AgentTicket{}, ErrAgentTicket
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var claims AgentTicket
	if decoder.Decode(&claims) != nil || decoder.Decode(new(any)) != io.EOF ||
		!validAgentTicketIdentity(claims) || claims.Audience != workspaceAgentAudience ||
		!validAgentPermission(claims.Permission) || !agentTicketIDPattern.MatchString(claims.TicketID) ||
		claims.WorkspaceID != expected.WorkspaceID || claims.TenantID != expected.TenantID ||
		claims.ResourcePlane != expected.ResourcePlane || claims.MicroVMID != expected.MicroVMID ||
		(expected.UserID != "" && claims.UserID != expected.UserID) || claims.Permission != expected.Permission || claims.Port != expected.Port ||
		!validAgentTarget(claims) {
		return AgentTicket{}, ErrAgentTicket
	}
	issued := time.Unix(claims.IssuedAt, 0).UTC()
	expires := time.Unix(claims.ExpiresAt, 0).UTC()
	if claims.Version != 1 || claims.IssuedAt <= 0 || !now.Before(expires) ||
		expires.Sub(issued) <= 0 || expires.Sub(issued) > workspaceAgentTicketTTL || issued.After(now.Add(5*time.Second)) {
		return AgentTicket{}, ErrAgentTicket
	}
	return claims, nil
}

func validAgentTicketIdentity(ticket AgentTicket) bool {
	return ticket.UserID != "" && agentIdentityPattern.MatchString(string(ticket.TenantID)) &&
		agentIdentityPattern.MatchString(string(ticket.WorkspaceID)) &&
		agentIdentityPattern.MatchString(string(ticket.ResourcePlane)) && agentIdentityPattern.MatchString(ticket.MicroVMID)
}

func validAgentTicketBinding(ticket AgentTicket) bool {
	return (ticket.UserID == "" || agentIdentityPattern.MatchString(string(ticket.UserID))) &&
		agentIdentityPattern.MatchString(string(ticket.TenantID)) &&
		agentIdentityPattern.MatchString(string(ticket.WorkspaceID)) &&
		agentIdentityPattern.MatchString(string(ticket.ResourcePlane)) && agentIdentityPattern.MatchString(ticket.MicroVMID)
}

func validAgentPermission(permission workspacesession.Permission) bool {
	switch permission {
	case workspacesession.PermissionShell, workspacesession.PermissionVSCode, workspacesession.PermissionPortForward, workspacesession.PermissionSSHProxy:
		return true
	default:
		return false
	}
}

func validAgentTarget(ticket AgentTicket) bool {
	if !validAgentPermission(ticket.Permission) {
		return false
	}
	switch ticket.Permission {
	case workspacesession.PermissionPortForward:
		return ticket.Port != 0
	case workspacesession.PermissionSSHProxy:
		return ticket.Port == 0
	default:
		return ticket.Port == 0
	}
}

// AgentTicketReplayGuard makes each signed Gateway capability one-use inside
// a MicroVM. It is intentionally memory-only; MicroVM replacement starts with
// an empty guard and also has a different pinned MicroVM identity.
type AgentTicketReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (g *AgentTicketReplayGuard) Consume(ticket AgentTicket, now time.Time) error {
	if g == nil || ticket.TicketID == "" || !now.Before(time.Unix(ticket.ExpiresAt, 0)) {
		return ErrAgentTicket
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
	if _, exists := g.seen[ticket.TicketID]; exists || len(g.seen) >= maxAgentReplayTickets {
		return ErrAgentTicket
	}
	g.seen[ticket.TicketID] = time.Unix(ticket.ExpiresAt, 0)
	return nil
}
