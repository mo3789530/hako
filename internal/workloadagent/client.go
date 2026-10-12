package workloadagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

const maxProtocolBody = 64 << 10
const sourceUploadTimeout = 10 * time.Minute

var protocolID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var protocolDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var protocolCommit = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var microVMEndpoint = regexp.MustCompile(`^[a-z0-9_-]+\.lambda-microvm\.[a-z0-9-]+\.on\.aws$`)

var (
	ErrProtocolRequest  = errors.New("invalid Hako Agent protocol request")
	ErrProtocolResponse = errors.New("invalid Hako Agent protocol response")
	ErrAgentTransport   = errors.New("Hako Agent transport failed")
)

// Client sends only bounded metadata to the endpoint. It never uses proxy
// environment variables, follows redirects, or includes provider errors or
// response bodies in returned errors (which may contain untrusted secrets).
type Client struct {
	http       *http.Client
	sourceHTTP *http.Client
	logsHTTP   *http.Client
}

func NewClient(transport http.RoundTripper) *Client {
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 nil,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   3 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConnsPerHost:   2,
			DisableCompression:    true,
		}
	}
	checkRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: checkRedirect},
		sourceHTTP: &http.Client{Transport: transport, Timeout: sourceUploadTimeout, CheckRedirect: checkRedirect},
		logsHTTP:   &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: checkRedirect}}
}

func (c *Client) Start(ctx context.Context, endpoint, authToken, agentTicket string, in StartRequest) error {
	if err := validateStart(in); err != nil {
		return err
	}
	_, err := c.call(ctx, http.MethodPut, endpoint, authToken, agentTicket, in.Identity.Command(), "", in, nil)
	return err
}

// UploadSource streams one content-addressed archive to the bound MicroVM.
// The Agent verifies the complete SHA-256 before extraction; the controller
// never buffers the repository tree or puts it in the RunHook payload.
func (c *Client) UploadSource(ctx context.Context, endpoint, authToken, sourceTicket string, id workloadjournal.Command, digest, commitSHA string, size int64, source io.Reader) error {
	if c == nil || c.sourceHTTP == nil || ctx == nil || !validIdentity(id) || !protocolDigest.MatchString(digest) ||
		!protocolCommit.MatchString(commitSHA) || size < 1 || size > maxSourceArchiveBytes || source == nil || authToken == "" || strings.ContainsAny(authToken, "\r\n") ||
		sourceTicket == "" || len(sourceTicket) > 8192 || strings.ContainsAny(sourceTicket, " \t\r\n") {
		return ErrProtocolRequest
	}
	u, err := endpointURL(endpoint)
	if err != nil {
		return err
	}
	segments := []string{"v1", "tenants", id.TenantID, "runs", id.WorkloadRunID, "attempts", id.AttemptID,
		"generations", fmt.Sprint(id.Generation), "resource-planes", id.ResourcePlaneID, "source"}
	u.Path = path.Join(append([]string{u.Path}, segments...)...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), io.LimitReader(source, size))
	if err != nil {
		return ErrProtocolRequest
	}
	req.ContentLength = size
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("X-Hako-Agent-Protocol", ProtocolVersion)
	req.Header.Set("X-Hako-Source-SHA256", digest)
	req.Header.Set("X-Hako-Commit-SHA", commitSHA)
	req.Header.Set("X-aws-proxy-auth", authToken)
	req.Header.Set("X-aws-proxy-port", "8080")
	req.Header.Set("Authorization", "Bearer "+sourceTicket)
	req.Header.Set("X-Hako-Idempotency-Token", id.IdempotencyToken)
	resp, err := c.sourceHTTP.Do(req)
	if err != nil {
		return ErrAgentTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Hako Agent returned HTTP status %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("X-Hako-Agent-Protocol") != ProtocolVersion {
		return ErrProtocolResponse
	}
	return nil
}

func (c *Client) Status(ctx context.Context, endpoint, authToken, agentTicket string, id workloadjournal.Command) (workloadjournal.ExecutionState, error) {
	var out StatusResponse
	if _, err := c.call(ctx, http.MethodGet, endpoint, authToken, agentTicket, id, "status", nil, &out); err != nil {
		return "", err
	}
	if out.Identity.Command() != id || !validReportedState(out.State) {
		return "", ErrProtocolResponse
	}
	return out.State, nil
}

func (c *Client) Result(ctx context.Context, endpoint, authToken, agentTicket string, id workloadjournal.Command) (workloadjobs.WorkloadResult, error) {
	var out ResultResponse
	if _, err := c.call(ctx, http.MethodGet, endpoint, authToken, agentTicket, id, "result", nil, &out); err != nil {
		return workloadjobs.WorkloadResult{}, err
	}
	if out.Identity.Command() != id || workloadjobs.ValidateResult(out.Result) != nil ||
		string(out.Result.TenantID) != id.TenantID || string(out.Result.WorkloadRunID) != id.WorkloadRunID ||
		out.Result.AttemptID != id.AttemptID || out.Result.Generation != id.Generation ||
		string(out.Result.ResourcePlaneID) != id.ResourcePlaneID || !validTerminalStatus(string(out.Result.Status)) {
		return workloadjobs.WorkloadResult{}, ErrProtocolResponse
	}
	return out.Result, nil
}

// Logs fetches only the bounded, Agent-redacted log artifact for a completed
// attempt. The Resource Plane caller can stream the returned bytes into its
// own S3 role; no AWS credentials need to be present in the MicroVM.
func (c *Client) Logs(ctx context.Context, endpoint, authToken, agentTicket string, id workloadjournal.Command) ([]byte, string, error) {
	if c == nil || c.logsHTTP == nil || ctx == nil || !validIdentity(id) || authToken == "" || strings.ContainsAny(authToken, "\r\n") ||
		agentTicket == "" || len(agentTicket) > 8192 || strings.ContainsAny(agentTicket, " \t\r\n") {
		return nil, "", ErrProtocolRequest
	}
	u, err := endpointURL(endpoint)
	if err != nil {
		return nil, "", err
	}
	segments := []string{"v1", "tenants", id.TenantID, "runs", id.WorkloadRunID, "attempts", id.AttemptID,
		"generations", strconv.FormatInt(id.Generation, 10), "resource-planes", id.ResourcePlaneID, "logs"}
	u.Path = path.Join(append([]string{u.Path}, segments...)...)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", ErrProtocolRequest
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("X-Hako-Agent-Protocol", ProtocolVersion)
	req.Header.Set("X-aws-proxy-auth", authToken)
	req.Header.Set("X-aws-proxy-port", "8080")
	req.Header.Set("Authorization", "Bearer "+agentTicket)
	req.Header.Set("X-Hako-Idempotency-Token", id.IdempotencyToken)
	resp, err := c.logsHTTP.Do(req)
	if err != nil {
		return nil, "", ErrAgentTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Hako Agent returned HTTP status %d", resp.StatusCode)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	digest := resp.Header.Get("X-Hako-Log-SHA256")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Hako-Agent-Protocol") != ProtocolVersion || mediaErr != nil ||
		mediaType != "text/plain" || resp.ContentLength < 0 || resp.ContentLength > maxAgentLogBytes || !protocolDigest.MatchString(digest) {
		return nil, "", fmt.Errorf("%w: invalid log response headers (status=%d protocol=%q media_type=%q length=%d digest_valid=%t)",
			ErrProtocolResponse, resp.StatusCode, resp.Header.Get("X-Hako-Agent-Protocol"), mediaType, resp.ContentLength, protocolDigest.MatchString(digest))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentLogBytes+1))
	if err != nil || int64(len(data)) != resp.ContentLength || len(data) > maxAgentLogBytes {
		return nil, "", fmt.Errorf("%w: invalid log response length", ErrProtocolResponse)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, "", fmt.Errorf("%w: log digest mismatch", ErrProtocolResponse)
	}
	return data, digest, nil
}

func (c *Client) call(ctx context.Context, method, endpoint, token, agentTicket string, id workloadjournal.Command, operation string, body any, out any) (int, error) {
	if c == nil || c.http == nil || ctx == nil || !validIdentity(id) || token == "" || strings.ContainsAny(token, "\r\n") ||
		len(agentTicket) == 0 || len(agentTicket) > 8192 || strings.ContainsAny(agentTicket, " \t\r\n") {
		return 0, ErrProtocolRequest
	}
	u, err := endpointURL(endpoint)
	if err != nil {
		return 0, err
	}
	segments := []string{"v1", "tenants", id.TenantID, "runs", id.WorkloadRunID, "attempts", id.AttemptID, "generations", fmt.Sprint(id.Generation), "resource-planes", id.ResourcePlaneID}
	if operation != "" {
		segments = append(segments, operation)
	}
	u.Path = path.Join(append([]string{u.Path}, segments...)...)
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil || len(encoded) > maxProtocolBody {
			return 0, ErrProtocolRequest
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return 0, ErrProtocolRequest
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Hako-Agent-Protocol", ProtocolVersion)
	req.Header.Set("X-aws-proxy-auth", token)
	req.Header.Set("X-aws-proxy-port", "8080")
	req.Header.Set("Authorization", "Bearer "+agentTicket)
	req.Header.Set("X-Hako-Idempotency-Token", id.IdempotencyToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, ErrAgentTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("Hako Agent returned HTTP status %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Hako-Agent-Protocol") != ProtocolVersion {
		return resp.StatusCode, ErrProtocolResponse
	}
	if out == nil {
		if resp.StatusCode != http.StatusNoContent {
			return resp.StatusCode, ErrProtocolResponse
		}
		return resp.StatusCode, nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return resp.StatusCode, ErrProtocolResponse
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxProtocolBody+1))
	if err != nil || len(data) > maxProtocolBody {
		return resp.StatusCode, ErrProtocolResponse
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(out) != nil || dec.Decode(new(any)) != io.EOF {
		return resp.StatusCode, ErrProtocolResponse
	}
	return resp.StatusCode, nil
}

func endpointURL(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || u.Scheme != "https" || !microVMEndpoint.MatchString(u.Hostname()) || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, ErrProtocolRequest
	}
	return u, nil
}

func validateStart(in StartRequest) error {
	if !validIdentity(in.Identity.Command()) || !protocolDigest.MatchString(in.SourceDigest) || !protocolCommit.MatchString(in.CommitSHA) || in.Deadline.IsZero() {
		return ErrProtocolRequest
	}
	return nil
}

func validIdentity(id workloadjournal.Command) bool {
	return protocolID.MatchString(id.TenantID) && protocolID.MatchString(id.WorkloadRunID) && protocolID.MatchString(id.AttemptID) &&
		protocolID.MatchString(id.ResourcePlaneID) && protocolID.MatchString(id.IdempotencyToken) && id.Generation > 0
}

func validReportedState(state workloadjournal.ExecutionState) bool {
	switch state {
	case workloadjournal.Running, workloadjournal.Succeeded, workloadjournal.Failed, workloadjournal.Cancelled, workloadjournal.TimedOut:
		return true
	default:
		return false
	}
}

func validTerminalStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "timed_out":
		return true
	default:
		return false
	}
}
