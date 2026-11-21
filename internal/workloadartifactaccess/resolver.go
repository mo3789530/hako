package workloadartifactaccess

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxArtifactSize = 100 << 20

var requestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

var ErrGrantUnavailable = errors.New("Workload artifact access is unavailable")

type Grant struct {
	StorageRef string `json:"storage_ref"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	Kind       string `json:"kind"`
}

type ResolverConfig struct {
	Endpoint string
	PlaneID  string
	HMACKey  []byte
	Client   *http.Client
	Now      func() time.Time
	Random   io.Reader
}

type Resolver struct {
	endpoint string
	planeID  string
	key      []byte
	client   *http.Client
	now      func() time.Time
	random   io.Reader
}

func NewResolver(config ResolverConfig) (*Resolver, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.Path != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		!resourcePlaneID.MatchString(config.PlaneID) || len(config.HMACKey) < 32 || len(config.HMACKey) > 4096 {
		return nil, ErrGrantUnavailable
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	random := config.Random
	if random == nil {
		random = rand.Reader
	}
	return &Resolver{endpoint: strings.TrimRight(endpoint.String(), "/"), planeID: config.PlaneID,
		key: append([]byte(nil), config.HMACKey...), client: client, now: now, random: random}, nil
}

func (r *Resolver) Resolve(ctx context.Context, claims Claims) (Grant, error) {
	if r == nil || r.client == nil || ctx == nil || !validIdentity(claims) || claims.ResourcePlane != r.planeID ||
		claims.Version != 1 || claims.Audience != Audience || !validTicketID(claims.TicketID) ||
		claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64(TicketMaxTTL/time.Second) ||
		!r.now().Before(time.Unix(claims.ExpiresAt, 0)) {
		return Grant{}, ErrGrantUnavailable
	}
	body, err := json.Marshal(struct {
		Claims Claims `json:"claims"`
	}{Claims: claims})
	if err != nil || len(body) > 8<<10 {
		return Grant{}, ErrGrantUnavailable
	}
	var nonce [16]byte
	if _, err := io.ReadFull(r.random, nonce[:]); err != nil {
		return Grant{}, ErrGrantUnavailable
	}
	requestID := "req_" + hex.EncodeToString(nonce[:])
	timestamp := strconv.FormatInt(r.now().UTC().Unix(), 10)
	path := "/v1/internal/resource-planes/" + url.PathEscape(r.planeID) + "/workload-artifacts/" + url.PathEscape(claims.ArtifactID) + "/access"
	u, err := url.Parse(r.endpoint + path)
	if err != nil {
		return Grant{}, ErrGrantUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Grant{}, ErrGrantUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hako-Timestamp", timestamp)
	req.Header.Set("X-Hako-Request-ID", requestID)
	req.Header.Set("X-Hako-Signature", signRequest(r.key, http.MethodPost, u.EscapedPath(), timestamp, requestID, body))
	response, err := r.client.Do(req)
	if err != nil {
		return Grant{}, ErrGrantUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Grant{}, ErrGrantUnavailable
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return Grant{}, ErrGrantUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if err != nil || len(data) == 0 || len(data) >= 16<<10 {
		return Grant{}, ErrGrantUnavailable
	}
	var grant Grant
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&grant) != nil || decoder.Decode(new(any)) != io.EOF || !validGrant(grant) {
		return Grant{}, ErrGrantUnavailable
	}
	return grant, nil
}

func validGrant(grant Grant) bool {
	parsed, err := url.Parse(grant.StorageRef)
	if err != nil || parsed.Scheme != "s3" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" ||
		grant.SizeBytes < 0 || grant.SizeBytes > MaxArtifactSize || len(grant.SHA256) != sha256.Size*2 || grant.Kind == "" {
		return false
	}
	if _, err := hex.DecodeString(grant.SHA256); err != nil || strings.ToLower(grant.SHA256) != grant.SHA256 {
		return false
	}
	for _, component := range strings.Split(parsed.Path, "/") {
		if component == ".." || component == "." {
			return false
		}
	}
	return true
}

func ValidateGrant(grant Grant) bool { return validGrant(grant) }

func signRequest(key []byte, method, path, timestamp, requestID string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	message := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", method, path, timestamp, requestID, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
