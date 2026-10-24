// Package workspacesession defines short-lived, audience-bound credentials for
// interactive Workspace Gateway connections. It is intentionally separate from
// workloadagent tickets, which authorize non-interactive CI operations.
package workspacesession

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const (
	AudienceGateway = "hako-gateway"
	TicketMaxTTL    = 5 * time.Minute
	maxTicketBytes  = 8192
)

var (
	ErrTicket       = errors.New("invalid Workspace Session Ticket")
	keyIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	resourceIDRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

type Permission string

const (
	PermissionShell        Permission = "shell"
	PermissionExec         Permission = "exec"
	PermissionVSCode       Permission = "vscode"
	PermissionPortForward  Permission = "port-forward"
	PermissionSSHProxy     Permission = "ssh-proxy"
	PermissionFileTransfer Permission = "file-transfer"
)

type Claims struct {
	Version       int                    `json:"version"`
	Audience      string                 `json:"audience"`
	TicketID      string                 `json:"ticket_id"`
	UserID        domain.UserID          `json:"user_id"`
	TenantID      domain.TenantID        `json:"tenant_id"`
	WorkspaceID   domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlane domain.ResourcePlaneID `json:"resource_plane_id"`
	Permissions   []Permission           `json:"permissions"`
	IssuedAt      int64                  `json:"issued_at"`
	ExpiresAt     int64                  `json:"expires_at"`
}

type Issuer interface {
	Issue(Claims) (string, Claims, error)
}

type Ed25519Issuer struct {
	KeyID      string
	PrivateKey ed25519.PrivateKey
	TTL        time.Duration
	Now        func() time.Time
	Random     io.Reader
}

func (i *Ed25519Issuer) Issue(claims Claims) (string, Claims, error) {
	if i == nil || !keyIDPattern.MatchString(i.KeyID) || len(i.PrivateKey) != ed25519.PrivateKeySize ||
		claims.UserID == "" || claims.TenantID == "" || claims.WorkspaceID == "" ||
		!resourceIDRegex.MatchString(string(claims.ResourcePlane)) || !validPermissions(claims.Permissions) {
		return "", Claims{}, ErrTicket
	}
	ttl := i.TTL
	if ttl <= 0 {
		ttl = TicketMaxTTL
	} else if ttl > TicketMaxTTL {
		return "", Claims{}, ErrTicket
	}
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	random := i.Random
	if random == nil {
		random = rand.Reader
	}
	var ticketID [24]byte
	if _, err := io.ReadFull(random, ticketID[:]); err != nil {
		return "", Claims{}, ErrTicket
	}
	issuedAt := now().UTC()
	claims.Version = 1
	claims.Audience = AudienceGateway
	claims.TicketID = hex.EncodeToString(ticketID[:])
	claims.IssuedAt = issuedAt.Unix()
	claims.ExpiresAt = issuedAt.Add(ttl).Unix()
	claims.Permissions = append([]Permission(nil), claims.Permissions...)
	sort.Slice(claims.Permissions, func(a, b int) bool { return claims.Permissions[a] < claims.Permissions[b] })
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > 4096 {
		return "", Claims{}, ErrTicket
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signed := i.KeyID + "." + encoded
	signature := ed25519.Sign(i.PrivateKey, []byte(signed))
	token := signed + "." + base64.RawURLEncoding.EncodeToString(signature)
	return token, claims, nil
}

func Verify(token string, keys map[string]ed25519.PublicKey, audience string, now time.Time) (Claims, error) {
	if len(token) == 0 || len(token) > maxTicketBytes || audience == "" {
		return Claims{}, ErrTicket
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !keyIDPattern.MatchString(parts[0]) {
		return Claims{}, ErrTicket
	}
	key := keys[parts[0]]
	if len(key) != ed25519.PublicKeySize {
		return Claims{}, ErrTicket
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > 4096 {
		return Claims{}, ErrTicket
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return Claims{}, ErrTicket
	}
	var claims Claims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claims) != nil || decoder.Decode(new(any)) != io.EOF || claims.Version != 1 || claims.Audience != audience ||
		len(claims.TicketID) != 48 || claims.UserID == "" || claims.TenantID == "" || claims.WorkspaceID == "" ||
		!resourceIDRegex.MatchString(string(claims.ResourcePlane)) || !validPermissions(claims.Permissions) {
		return Claims{}, ErrTicket
	}
	if ticketID, err := hex.DecodeString(claims.TicketID); err != nil || len(ticketID) != 24 {
		return Claims{}, ErrTicket
	}
	if now.IsZero() {
		now = time.Now()
	}
	issuedAt, expiresAt := time.Unix(claims.IssuedAt, 0), time.Unix(claims.ExpiresAt, 0)
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || expiresAt.Sub(issuedAt) > TicketMaxTTL ||
		issuedAt.After(now.Add(30*time.Second)) || !now.Before(expiresAt) || now.Sub(issuedAt) > TicketMaxTTL+30*time.Second {
		return Claims{}, ErrTicket
	}
	return claims, nil
}

func validPermissions(permissions []Permission) bool {
	if len(permissions) == 0 || len(permissions) > 6 {
		return false
	}
	seen := make(map[Permission]struct{}, len(permissions))
	for _, permission := range permissions {
		switch permission {
		case PermissionShell, PermissionExec, PermissionVSCode, PermissionPortForward, PermissionSSHProxy, PermissionFileTransfer:
		default:
			return false
		}
		if _, exists := seen[permission]; exists {
			return false
		}
		seen[permission] = struct{}{}
	}
	return true
}

func ValidatePermissions(permissions []Permission) error {
	if !validPermissions(permissions) {
		return ErrTicket
	}
	return nil
}

// AuthorizeChannel checks a channel against signed permissions. Routing must
// still use the signed Claims fields, never a caller-supplied workspace ID.
func AuthorizeChannel(claims Claims, permission Permission) error {
	if !validPermissions(claims.Permissions) {
		return ErrTicket
	}
	for _, allowed := range claims.Permissions {
		if allowed == permission {
			return nil
		}
	}
	return ErrTicket
}
