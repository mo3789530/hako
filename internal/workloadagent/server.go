package workloadagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

var (
	ErrAttemptConflict = errors.New("Workload attempt identity conflicts with an existing agent process")
	ErrAttemptNotFound = errors.New("Workload attempt is not known to the agent")
	ErrAgentBusy       = errors.New("Hako Agent is temporarily unavailable")
	ErrSourceNotReady  = errors.New("verified Workload source is not installed")
)

const maxSourceArchiveBytes = 1 << 30
const maxAgentLogBytes = 16 << 20
const maxAgentChangeBundleBytes = 8 << 20

// Backend owns idempotency and process lifecycle. It must pin a single full
// attempt identity to the MicroVM ID in the verified ticket and never execute
// repository-provided shell text as part of StartRequest.
type Backend interface {
	Start(context.Context, TicketClaims, StartRequest) error
	Status(context.Context, TicketClaims) (workloadjournal.ExecutionState, error)
	Result(context.Context, TicketClaims) (workloadjobs.WorkloadResult, error)
	Logs(context.Context, TicketClaims) ([]byte, error)
}

type changeBundleBackend interface {
	ChangeBundle(context.Context, TicketClaims) ([]byte, error)
}

type SourceInstaller interface {
	Install(context.Context, TicketClaims, string, string, io.Reader, int64) error
	Ready(context.Context, TicketClaims, string, string) bool
}

type Server struct {
	backend           Backend
	sources           SourceInstaller
	keys              map[string]ed25519.PublicKey
	now               func() time.Time
	requireRuntimePin bool
	runtimeBindingMu  sync.RWMutex
	runtimeBinding    *RuntimeBinding
}

func NewHandler(backend Backend, sources SourceInstaller, keys map[string]ed25519.PublicKey, now func() time.Time) (http.Handler, error) {
	server, err := newServer(backend, sources, keys, now, false)
	if err != nil {
		return nil, err
	}
	return server.Handler(), nil
}

func newServer(backend Backend, sources SourceInstaller, keys map[string]ed25519.PublicKey, now func() time.Time, requireRuntimePin bool) (*Server, error) {
	if backend == nil || sources == nil || len(keys) == 0 {
		return nil, ErrProtocolRequest
	}
	trusted := make(map[string]ed25519.PublicKey, len(keys))
	for id, key := range keys {
		if !keyIDPattern.MatchString(id) || len(key) != ed25519.PublicKeySize {
			return nil, ErrProtocolRequest
		}
		trusted[id] = append(ed25519.PublicKey(nil), key...)
	}
	if now == nil {
		now = time.Now
	}
	return &Server{backend: backend, sources: sources, keys: trusted, now: now, requireRuntimePin: requireRuntimePin}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", s.handle)
	return mux
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Hako-Agent-Protocol") != ProtocolVersion {
		http.Error(w, "unsupported Hako Agent protocol", http.StatusUpgradeRequired)
		return
	}
	id, operation, err := routeIdentity(r)
	if err != nil {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	claims, err := VerifyTicketClaims(bearer(r.Header.Get("Authorization")), s.keys, id, operation, s.now().UTC())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.requireRuntimePin {
		binding := s.currentRuntimeBinding()
		if binding == nil {
			http.Error(w, "agent not initialized", http.StatusServiceUnavailable)
			return
		}
		if claims.MicroVMID != binding.MicroVMID || claims.Identity != binding.Identity ||
			(claims.Execution != nil && !sameExecutionBinding(*claims.Execution, binding.Execution)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	switch operation {
	case OperationStart:
		s.start(w, r, claims)
	case OperationSource:
		s.source(w, r, claims)
	case OperationStatus:
		s.status(w, r, claims)
	case OperationResult:
		s.result(w, r, claims)
	case OperationLogs:
		s.logs(w, r, claims)
	case OperationChanges:
		s.changes(w, r, claims)
	default:
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
	}
}

func (s *Server) start(w http.ResponseWriter, r *http.Request, claims TicketClaims) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("X-Hako-Idempotency-Token") != claims.Identity.IdempotencyToken {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.ContentLength < 0 {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	if r.ContentLength > maxProtocolBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProtocolBody+1))
	if err != nil || len(body) > maxProtocolBody {
		clear(body)
		http.Error(w, "invalid Hako Agent request", http.StatusRequestEntityTooLarge)
		return
	}
	defer clear(body)
	var input StartRequest
	defer func() { clear(input.AgentModelCredential) }()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || validateStart(input) != nil || input.Identity.Command() != claims.Identity.Command() {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	if claims.Execution == nil || input.Kind != claims.Execution.Kind || input.SourceDigest != claims.Execution.SourceDigest || input.CommitSHA != claims.Execution.CommitSHA ||
		!input.Deadline.Equal(claims.Execution.Deadline) {
		http.Error(w, "request does not match signed execution ticket", http.StatusUnauthorized)
		return
	}
	deadlineIn := input.Deadline.Sub(s.now())
	if deadlineIn <= 0 || deadlineIn > 8*time.Hour {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	if !s.sources.Ready(r.Context(), claims, input.SourceDigest, input.CommitSHA) {
		writeBackendError(w, ErrSourceNotReady)
		return
	}
	if err := s.backend.Start(r.Context(), claims, input); err != nil {
		writeBackendError(w, err)
		return
	}
	w.Header().Set("X-Hako-Agent-Protocol", ProtocolVersion)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) source(w http.ResponseWriter, r *http.Request, claims TicketClaims) {
	if r.ContentLength < 1 || r.ContentLength > maxSourceArchiveBytes || len(r.TransferEncoding) != 0 ||
		r.Header.Get("X-Hako-Idempotency-Token") != claims.Identity.IdempotencyToken ||
		!protocolDigest.MatchString(r.Header.Get("X-Hako-Source-SHA256")) ||
		!protocolCommit.MatchString(r.Header.Get("X-Hako-Commit-SHA")) || claims.Execution == nil ||
		r.Header.Get("X-Hako-Source-SHA256") != claims.Execution.SourceDigest ||
		r.Header.Get("X-Hako-Commit-SHA") != claims.Execution.CommitSHA {
		http.Error(w, "invalid Hako Agent source request", http.StatusBadRequest)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-tar" {
		http.Error(w, "invalid Hako Agent source request", http.StatusBadRequest)
		return
	}
	if err := s.sources.Install(r.Context(), claims, r.Header.Get("X-Hako-Source-SHA256"), r.Header.Get("X-Hako-Commit-SHA"), r.Body, r.ContentLength); err != nil {
		writeBackendError(w, err)
		return
	}
	w.Header().Set("X-Hako-Agent-Protocol", ProtocolVersion)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, claims TicketClaims) {
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	state, err := s.backend.Status(r.Context(), claims)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if !validReportedState(state) {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, StatusResponse{Identity: claims.Identity, State: state})
}

func (s *Server) result(w http.ResponseWriter, r *http.Request, claims TicketClaims) {
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	result, err := s.backend.Result(r.Context(), claims)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	id := claims.Identity
	if workloadjobs.ValidateResult(result) != nil || string(result.TenantID) != id.TenantID || string(result.WorkloadRunID) != id.WorkloadRunID ||
		result.AttemptID != id.AttemptID || result.Generation != id.Generation || string(result.ResourcePlaneID) != id.ResourcePlaneID {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, ResultResponse{Identity: id, Result: result})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, claims TicketClaims) {
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	data, err := s.backend.Logs(r.Context(), claims)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if len(data) > maxAgentLogBytes {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
		return
	}
	digest := sha256.Sum256(data)
	w.Header().Set("X-Hako-Agent-Protocol", ProtocolVersion)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Hako-Log-SHA256", hex.EncodeToString(digest[:]))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request, claims TicketClaims) {
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 || claims.Execution == nil || claims.Execution.Kind != "agent" {
		http.Error(w, "invalid Hako Agent request", http.StatusBadRequest)
		return
	}
	backend, ok := s.backend.(changeBundleBackend)
	if !ok {
		http.Error(w, "change bundle unavailable", http.StatusNotFound)
		return
	}
	data, err := backend.ChangeBundle(r.Context(), claims)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	defer clear(data)
	if len(data) == 0 {
		w.Header().Set("X-Hako-Agent-Protocol", ProtocolVersion)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(data) > maxAgentChangeBundleBytes {
		http.Error(w, "change bundle unavailable", http.StatusServiceUnavailable)
		return
	}
	digest := sha256.Sum256(data)
	w.Header().Set("X-Hako-Agent-Protocol", ProtocolVersion)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Hako-Change-SHA256", hex.EncodeToString(digest[:]))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func routeIdentity(r *http.Request) (workloadjournal.Command, Operation, error) {
	if r.URL.RawQuery != "" || r.URL.Fragment != "" {
		return workloadjournal.Command{}, "", ErrProtocolRequest
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 11 && len(parts) != 12 {
		return workloadjournal.Command{}, "", ErrProtocolRequest
	}
	if parts[0] != "v1" || parts[1] != "tenants" || parts[3] != "runs" || parts[5] != "attempts" || parts[7] != "generations" || parts[9] != "resource-planes" {
		return workloadjournal.Command{}, "", ErrProtocolRequest
	}
	generation, err := strconv.ParseInt(parts[8], 10, 64)
	if err != nil || generation < 1 {
		return workloadjournal.Command{}, "", ErrProtocolRequest
	}
	identity := workloadjournal.Command{TenantID: parts[2], WorkloadRunID: parts[4], AttemptID: parts[6], Generation: generation,
		ResourcePlaneID: parts[10], IdempotencyToken: r.Header.Get("X-Hako-Idempotency-Token")}
	if !validIdentity(identity) {
		return workloadjournal.Command{}, "", ErrProtocolRequest
	}
	if r.Method == http.MethodPut && len(parts) == 11 {
		return identity, OperationStart, nil
	}
	if r.Method == http.MethodPut && len(parts) == 12 && parts[11] == "source" {
		return identity, OperationSource, nil
	}
	if r.Method == http.MethodGet && len(parts) == 12 {
		switch parts[11] {
		case "status":
			return identity, OperationStatus, nil
		case "result":
			return identity, OperationResult, nil
		case "logs":
			return identity, OperationLogs, nil
		case "changes":
			return identity, OperationChanges, nil
		}
	}
	return workloadjournal.Command{}, "", ErrProtocolRequest
}

func bearer(value string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return strings.TrimPrefix(value, prefix)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maxProtocolBody {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("X-Hako-Agent-Protocol", ProtocolVersion)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}

func writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAttemptConflict):
		http.Error(w, "attempt identity conflict", http.StatusConflict)
	case errors.Is(err, ErrAttemptNotFound):
		http.Error(w, "attempt not found", http.StatusNotFound)
	case errors.Is(err, ErrSourceNotReady):
		http.Error(w, "source archive not installed", http.StatusConflict)
	case errors.Is(err, ErrAgentBusy):
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
	default:
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
	}
}
