package workloadagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

const (
	microVMLifecyclePrefix = "/aws/lambda-microvms/runtime/v1/"
	maxRunHookPayloadBytes = 16 << 10
)

// RuntimeBinding is the immutable per-allocation identity delivered by the
// trusted Lambda MicroVM /run hook. It contains no credentials or secret data.
type RuntimeBinding struct {
	MicroVMID string
	Identity  Identity
	Execution ExecutionBinding
}

type runtimeHookPayload struct {
	Identity      workloadjournal.Command `json:"identity"`
	SourceDigest  string                  `json:"source_digest"`
	CommitSHA     string                  `json:"commit_sha"`
	TriggerEvent  string                  `json:"trigger_event,omitempty"`
	TriggerBranch string                  `json:"trigger_branch,omitempty"`
	Deadline      time.Time               `json:"deadline"`
}

type runtimeHookRequest struct {
	MicroVMID      string `json:"microvmId"`
	RunHookPayload string `json:"runHookPayload"`
}

// BindRuntime pins an allocation once. Repeated identical /run hook delivery
// is idempotent; a different VM, attempt, source, or deadline is rejected.
func (s *Server) BindRuntime(microVMID string, identity workloadjournal.Command, execution ExecutionBinding) error {
	if s == nil || !protocolID.MatchString(microVMID) || !validIdentity(identity) || !validExecutionBinding(execution) {
		return ErrProtocolRequest
	}
	want := &RuntimeBinding{MicroVMID: microVMID, Identity: IdentityFromCommand(identity), Execution: execution}
	s.runtimeBindingMu.Lock()
	defer s.runtimeBindingMu.Unlock()
	if s.runtimeBinding != nil {
		if sameRuntimeBinding(*s.runtimeBinding, *want) {
			return nil
		}
		return ErrAttemptConflict
	}
	s.runtimeBinding = want
	return nil
}

func (s *Server) currentRuntimeBinding() *RuntimeBinding {
	if s == nil {
		return nil
	}
	s.runtimeBindingMu.RLock()
	defer s.runtimeBindingMu.RUnlock()
	if s.runtimeBinding == nil {
		return nil
	}
	binding := *s.runtimeBinding
	return &binding
}

func sameRuntimeBinding(left, right RuntimeBinding) bool {
	return left.MicroVMID == right.MicroVMID && left.Identity == right.Identity && sameExecutionBinding(left.Execution, right.Execution)
}

func sameExecutionBinding(left, right ExecutionBinding) bool {
	return left.SourceDigest == right.SourceDigest && left.CommitSHA == right.CommitSHA && left.TriggerEvent == right.TriggerEvent &&
		left.TriggerBranch == right.TriggerBranch && left.Deadline.Equal(right.Deadline)
}

// NewMicroVMLifecycleHandler exposes the documented Lambda MicroVM image and
// run hooks alongside the versioned Agent protocol. The /run payload is
// bounded, strict, and pinned before the provider can forward external traffic.
func NewMicroVMLifecycleHandler(next http.Handler, binder interface {
	BindRuntime(string, workloadjournal.Command, ExecutionBinding) error
}) (http.Handler, error) {
	if next == nil || binder == nil {
		return nil, ErrProtocolRequest
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", next)
	mux.HandleFunc(microVMLifecyclePrefix+"ready", lifecycleReady)
	mux.HandleFunc(microVMLifecyclePrefix+"validate", lifecycleReady)
	mux.HandleFunc(microVMLifecyclePrefix+"run", func(w http.ResponseWriter, r *http.Request) {
		lifecycleRun(w, r, binder)
	})
	mux.Handle("/", next)
	return mux, nil
}

func lifecycleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		http.Error(w, "invalid MicroVM lifecycle request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func lifecycleRun(w http.ResponseWriter, r *http.Request, binder interface {
	BindRuntime(string, workloadjournal.Command, ExecutionBinding) error
}) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" || r.URL.Fragment != "" || r.ContentLength > maxRunHookPayloadBytes || len(r.TransferEncoding) > 0 {
		http.Error(w, "invalid MicroVM lifecycle request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRunHookPayloadBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxRunHookPayloadBytes {
		http.Error(w, "invalid MicroVM lifecycle request", http.StatusBadRequest)
		return
	}
	var input runtimeHookRequest
	if decodeStrictJSON(body, &input) != nil || !protocolID.MatchString(input.MicroVMID) || strings.TrimSpace(input.RunHookPayload) == "" || len(input.RunHookPayload) > maxRunHookPayloadBytes {
		http.Error(w, "invalid MicroVM lifecycle request", http.StatusBadRequest)
		return
	}
	var payload runtimeHookPayload
	if decodeStrictJSON([]byte(input.RunHookPayload), &payload) != nil || !validIdentity(payload.Identity) {
		http.Error(w, "invalid MicroVM lifecycle request", http.StatusBadRequest)
		return
	}
	execution := ExecutionBinding{SourceDigest: payload.SourceDigest, CommitSHA: payload.CommitSHA,
		TriggerEvent: payload.TriggerEvent, TriggerBranch: payload.TriggerBranch, Deadline: payload.Deadline}
	if !validExecutionBinding(execution) {
		http.Error(w, "invalid MicroVM lifecycle request", http.StatusBadRequest)
		return
	}
	if err := binder.BindRuntime(input.MicroVMID, payload.Identity, execution); err != nil {
		if errors.Is(err, ErrAttemptConflict) {
			http.Error(w, "MicroVM runtime identity conflict", http.StatusConflict)
			return
		}
		http.Error(w, "invalid MicroVM lifecycle request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrProtocolRequest
	}
	return nil
}
