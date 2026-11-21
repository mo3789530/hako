package workloadagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type memoryAgentBackend struct {
	identity workloadjournal.Command
	vmID     string
	start    StartRequest
	starts   int
	state    workloadjournal.ExecutionState
	result   workloadjobs.WorkloadResult
	logs     []byte
	changes  []byte
	err      error
}

func (b *memoryAgentBackend) ChangeBundle(_ context.Context, claims TicketClaims) ([]byte, error) {
	if claims.Operation != OperationChanges || claims.Identity.Command() != b.identity || claims.MicroVMID != b.vmID {
		return nil, ErrAttemptNotFound
	}
	return append([]byte(nil), b.changes...), nil
}

type memorySourceInstaller struct {
	identity workloadjournal.Command
	vmID     string
	digest   string
	commit   string
	body     []byte
	installs int
}

func (s *memorySourceInstaller) Install(_ context.Context, claims TicketClaims, digest, commit string, body io.Reader, size int64) error {
	data, err := io.ReadAll(body)
	if err != nil || int64(len(data)) != size {
		return ErrSourceInstall
	}
	digestSum := sha256.Sum256(data)
	if hex.EncodeToString(digestSum[:]) != digest || !protocolCommit.MatchString(commit) {
		return ErrSourceInstall
	}
	if s.installs > 0 {
		if claims.Identity.Command() != s.identity || claims.MicroVMID != s.vmID || digest != s.digest || commit != s.commit || !bytes.Equal(data, s.body) {
			return ErrAttemptConflict
		}
		return nil
	}
	s.identity, s.vmID, s.digest, s.commit, s.body = claims.Identity.Command(), claims.MicroVMID, digest, commit, data
	s.installs++
	return nil
}

func (s *memorySourceInstaller) Ready(_ context.Context, claims TicketClaims, digest, commit string) bool {
	return s.installs > 0 && claims.Identity.Command() == s.identity && claims.MicroVMID == s.vmID && digest == s.digest && commit == s.commit
}

func (b *memoryAgentBackend) Start(_ context.Context, claims TicketClaims, req StartRequest) error {
	if b.err != nil {
		return b.err
	}
	if b.starts > 0 {
		if claims.Identity.Command() != b.identity || claims.MicroVMID != b.vmID || !reflect.DeepEqual(req, b.start) {
			return ErrAttemptConflict
		}
		return nil
	}
	b.identity, b.vmID, b.start = claims.Identity.Command(), claims.MicroVMID, req
	b.starts++
	b.state = workloadjournal.Running
	return nil
}

func (b *memoryAgentBackend) Status(_ context.Context, claims TicketClaims) (workloadjournal.ExecutionState, error) {
	if b.err != nil {
		return "", b.err
	}
	if claims.Identity.Command() != b.identity || claims.MicroVMID != b.vmID {
		return "", ErrAttemptNotFound
	}
	return b.state, nil
}

func (b *memoryAgentBackend) Result(_ context.Context, claims TicketClaims) (workloadjobs.WorkloadResult, error) {
	if b.err != nil {
		return workloadjobs.WorkloadResult{}, b.err
	}
	if claims.Identity.Command() != b.identity || claims.MicroVMID != b.vmID {
		return workloadjobs.WorkloadResult{}, ErrAttemptNotFound
	}
	return b.result, nil
}

func (b *memoryAgentBackend) Logs(_ context.Context, claims TicketClaims) ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	if claims.Identity.Command() != b.identity || claims.MicroVMID != b.vmID {
		return nil, ErrAttemptNotFound
	}
	return append([]byte(nil), b.logs...), nil
}

type handlerRoundTripper struct{ handler http.Handler }

func (t handlerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	t.handler.ServeHTTP(response, r)
	return response.Result(), nil
}

func TestAgentProtocolClientAndAuthenticatedServerLifecycle(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	backend := &memoryAgentBackend{}
	sources := &memorySourceInstaller{}
	handler, err := NewHandler(backend, sources, map[string]ed25519.PublicKey{"rp-v1": publicKey}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(handlerRoundTripper{handler})
	issuer := &Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	id := testIdentity()
	endpoint := "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws"
	issue := func(op Operation, execution ...ExecutionBinding) string {
		t.Helper()
		var token string
		var err error
		if len(execution) == 1 {
			token, err = issuer.IssueExecution("microvm_1", id, op, execution[0])
		} else {
			token, err = issuer.Issue("microvm_1", id, op)
		}
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	bind := func(start StartRequest) ExecutionBinding {
		return ExecutionBinding{SourceDigest: start.SourceDigest, CommitSHA: start.CommitSHA, Deadline: start.Deadline.UTC()}
	}
	start := StartRequest{Identity: IdentityFromCommand(id), SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)}
	archive := []byte("verified source")
	digestSum := sha256.Sum256(archive)
	start.SourceDigest = hex.EncodeToString(digestSum[:])
	binding := bind(start)
	startTicket := issue(OperationStart, binding)
	if err := client.UploadSource(context.Background(), endpoint, "aws-endpoint-token", issue(OperationSource, binding), id, start.SourceDigest, start.CommitSHA, int64(len(archive)), bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	changedExecution := start
	changedExecution.CommitSHA = strings.Repeat("c", 40)
	if err := client.Start(context.Background(), endpoint, "aws-endpoint-token", startTicket, changedExecution); err == nil || backend.starts != 0 {
		t.Fatalf("Start accepted body values outside its signed execution binding: starts=%d err=%v", backend.starts, err)
	}
	if err := client.Start(context.Background(), endpoint, "aws-endpoint-token", startTicket, start); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background(), endpoint, "aws-endpoint-token", startTicket, start); err != nil || backend.starts != 1 {
		t.Fatalf("duplicate start was not idempotent: calls=%d err=%v", backend.starts, err)
	}
	if state, err := client.Status(context.Background(), endpoint, "aws-endpoint-token", issue(OperationStatus), id); err != nil || state != workloadjournal.Running {
		t.Fatalf("Status() = %q, %v", state, err)
	}
	backend.state = workloadjournal.Succeeded
	backend.result = workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: domain.TenantID(id.TenantID), WorkloadRunID: domain.WorkloadRunID(id.WorkloadRunID),
		AttemptID: id.AttemptID, Generation: id.Generation, ResourcePlaneID: domain.ResourcePlaneID(id.ResourcePlaneID), CommitSHA: start.CommitSHA,
		Status: domain.WorkloadSucceeded, CompletedAt: now.Add(time.Minute)}
	if result, err := client.Result(context.Background(), endpoint, "aws-endpoint-token", issue(OperationResult), id); err != nil || result.Status != domain.WorkloadSucceeded {
		t.Fatalf("Result() = %+v, %v", result, err)
	}
	backend.logs = []byte("redacted test output\n")
	logSum := sha256.Sum256(backend.logs)
	if data, digest, err := client.Logs(context.Background(), endpoint, "aws-endpoint-token", issue(OperationLogs), id); err != nil || string(data) != string(backend.logs) || digest != hex.EncodeToString(logSum[:]) {
		t.Fatalf("Logs() = %q digest=%q err=%v", data, digest, err)
	}
	if backend.vmID != "microvm_1" || backend.identity != id {
		t.Fatal("backend did not pin the authenticated ticket to the attempt")
	}
}

func TestAgentChangeBundleEndpointRequiresBoundAgentTicketAndChecksDigest(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC)
	id := testIdentity()
	bundle := []byte("proposal bundle")
	backend := &memoryAgentBackend{identity: id, vmID: "microvm_1", state: workloadjournal.Succeeded, changes: bundle}
	handler, err := NewHandler(backend, &memorySourceInstaller{}, map[string]ed25519.PublicKey{"rp-v1": publicKey}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(handlerRoundTripper{handler})
	issuer := &Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	binding := ExecutionBinding{Kind: domain.WorkloadAgent, AgentIntegrationID: "integration_1", AgentInstallationID: 71,
		AgentRepositoryID: 81, AgentTaskContextSHA256: strings.Repeat("c", 64), SourceDigest: strings.Repeat("a", 64),
		CommitSHA: strings.Repeat("b", 40), TriggerEvent: "issue_comment", TriggerBranch: "main", Deadline: now.Add(time.Hour)}
	ticket, err := issuer.IssueExecution("microvm_1", id, OperationChanges, binding)
	if err != nil {
		t.Fatal(err)
	}
	data, digest, err := client.Changes(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "aws-token", ticket, id)
	actual := sha256.Sum256(bundle)
	if err != nil || !bytes.Equal(data, bundle) || digest != hex.EncodeToString(actual[:]) {
		t.Fatalf("change bundle fetch = %q, %q, %v", data, digest, err)
	}
	if _, _, err := client.Changes(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "aws-token",
		mustIssue(t, issuer, "microvm_1", id, OperationLogs), id); err == nil {
		t.Fatal("log ticket was accepted by the change bundle endpoint")
	}
}

func TestAgentSourceUploadMustBeOperationAuthenticatedAndVerifiedBeforeStart(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	backend := &memoryAgentBackend{}
	sources := &memorySourceInstaller{}
	handler, err := NewHandler(backend, sources, map[string]ed25519.PublicKey{"rp-v1": publicKey}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(handlerRoundTripper{handler})
	issuer := &Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	id := testIdentity()
	endpoint := "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws"
	archive := []byte("signed-and-digest-checked-source")
	digestSum := sha256.Sum256(archive)
	digest := hex.EncodeToString(digestSum[:])
	start := StartRequest{Identity: IdentityFromCommand(id), SourceDigest: digest, CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)}
	binding := ExecutionBinding{SourceDigest: digest, CommitSHA: start.CommitSHA, Deadline: start.Deadline.UTC()}
	startTicket := mustIssue(t, issuer, "microvm_1", id, OperationStart, binding)
	if err := client.Start(context.Background(), endpoint, "aws-endpoint-token", startTicket, start); err == nil {
		t.Fatal("process started before source archive was installed")
	}
	if backend.starts != 0 {
		t.Fatal("backend observed a Start request without source")
	}
	sourceTicket := mustIssue(t, issuer, "microvm_1", id, OperationSource, binding)
	if err := client.UploadSource(context.Background(), endpoint, "aws-endpoint-token", sourceTicket, id, digest, start.CommitSHA, int64(len(archive)), bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	if sources.installs != 1 || !bytes.Equal(sources.body, archive) {
		t.Fatal("source upload did not reach the authenticated installer")
	}
	if err := client.Start(context.Background(), endpoint, "aws-endpoint-token", startTicket, start); err != nil || backend.starts != 1 {
		t.Fatalf("Start after source install: calls=%d err=%v", backend.starts, err)
	}
	if err := client.UploadSource(context.Background(), endpoint, "aws-endpoint-token", startTicket, id, digest, start.CommitSHA, int64(len(archive)), bytes.NewReader(archive)); err == nil {
		t.Fatal("Start-operation ticket was accepted for source upload")
	}
	if err := client.UploadSource(context.Background(), endpoint, "aws-endpoint-token", sourceTicket, id, strings.Repeat("0", 64), start.CommitSHA, int64(len(archive)), bytes.NewReader(archive)); err == nil {
		t.Fatal("digest-mismatched source was accepted by the installer")
	}
	if backend.starts != 1 || sources.installs != 1 {
		t.Fatal("invalid upload changed the running attempt")
	}
}

func TestAgentServerRejectsUnauthenticatedMalformedAndUnsupportedRequests(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	backend := &memoryAgentBackend{}
	handler, err := NewHandler(backend, &memorySourceInstaller{}, map[string]ed25519.PublicKey{"rp-v1": publicKey}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity()
	issuer := &Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	ticket, err := issuer.Issue("microvm_1", id, OperationStatus)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/tenants/tenant_1/runs/run_1/attempts/attempt_1/generations/2/resource-planes/rp_1/status"
	for _, tc := range []struct {
		name     string
		version  string
		auth     string
		path     string
		wantCode int
	}{
		{name: "missing credential", version: ProtocolVersion, path: base, wantCode: http.StatusUnauthorized},
		{name: "unsupported protocol", version: "99", auth: "Bearer " + ticket, path: base, wantCode: http.StatusUpgradeRequired},
		{name: "malformed path", version: ProtocolVersion, auth: "Bearer " + ticket, path: "/v1/other", wantCode: http.StatusBadRequest},
		{name: "ticket bound to other operation", version: ProtocolVersion, auth: "Bearer " + mustIssue(t, issuer, "microvm_1", id, OperationResult), path: base, wantCode: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-Hako-Agent-Protocol", tc.version)
			req.Header.Set("X-Hako-Idempotency-Token", id.IdempotencyToken)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d, body=%q; want %d", rr.Code, rr.Body.String(), tc.wantCode)
			}
		})
	}
}

func TestAgentServerBoundsStrictStartBodyAndRedactsBackendErrors(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	backend := &memoryAgentBackend{err: errors.New("backend contained secret-value")}
	readySource := &memorySourceInstaller{identity: testIdentity(), vmID: "microvm_1", digest: strings.Repeat("a", 64), commit: strings.Repeat("b", 40), installs: 1}
	handler, err := NewHandler(backend, readySource, map[string]ed25519.PublicKey{"rp-v1": publicKey}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity()
	issuer := &Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	ticket := mustIssue(t, issuer, "microvm_1", id, OperationStart, ExecutionBinding{SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)})
	base := "/v1/tenants/tenant_1/runs/run_1/attempts/attempt_1/generations/2/resource-planes/rp_1"
	t.Run("oversized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, base, strings.NewReader(strings.Repeat("x", maxProtocolBody+1)))
		req.Header.Set("X-Hako-Agent-Protocol", ProtocolVersion)
		req.Header.Set("X-Hako-Idempotency-Token", id.IdempotencyToken)
		req.Header.Set("Authorization", "Bearer "+ticket)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized request status = %d", rr.Code)
		}
	})
	t.Run("backend error detail hidden", func(t *testing.T) {
		body, err := json.Marshal(StartRequest{Identity: IdentityFromCommand(id), SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, base, bytesReader(body))
		req.Header.Set("X-Hako-Agent-Protocol", ProtocolVersion)
		req.Header.Set("X-Hako-Idempotency-Token", id.IdempotencyToken)
		req.Header.Set("Authorization", "Bearer "+ticket)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable || strings.Contains(rr.Body.String(), "secret-value") {
			t.Fatalf("unsafe backend error response: code=%d body=%q", rr.Code, rr.Body.String())
		}
	})
}

func mustIssue(t *testing.T, issuer *Ed25519TicketIssuer, vm string, id workloadjournal.Command, op Operation, execution ...ExecutionBinding) string {
	t.Helper()
	var token string
	var err error
	if len(execution) == 1 {
		token, err = issuer.IssueExecution(vm, id, op, execution[0])
	} else {
		token, err = issuer.Issue(vm, id, op)
	}
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func bytesReader(body []byte) io.Reader { return strings.NewReader(string(body)) }
