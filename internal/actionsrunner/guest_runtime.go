package actionsrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const runnerMicroVMLifecyclePrefix = "/aws/lambda-microvms/runtime/v1/"

type runnerMicroVMRunHookRequest struct {
	MicroVMID      string `json:"microvmId"`
	RunHookPayload string `json:"runHookPayload"`
}

// RunnerGuestRuntime binds the provider's authenticated /run payload to the
// signed bootstrap receiver before Lambda MicroVM forwards external traffic.
// The payload carries only non-secret job identity; credentials arrive later
// over the one-time authenticated bootstrap route.
type RunnerGuestRuntime struct {
	publicKey ed25519.PublicKey
	consumer  RunnerBootstrapConsumer

	mu       sync.RWMutex
	vmID     string
	launch   RunnerLaunchPayload
	receiver http.Handler
}

func NewRunnerGuestRuntime(publicKey ed25519.PublicKey, consumer RunnerBootstrapConsumer) (*RunnerGuestRuntime, error) {
	if len(publicKey) != ed25519.PublicKeySize || consumer == nil {
		return nil, ErrRunnerBootstrap
	}
	return &RunnerGuestRuntime{publicKey: append(ed25519.PublicKey(nil), publicKey...), consumer: consumer}, nil
}

func (g *RunnerGuestRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g == nil || r == nil || r.URL == nil {
		http.Error(w, "runner guest unavailable", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case runnerMicroVMLifecyclePrefix + "ready", runnerMicroVMLifecyclePrefix + "validate":
		if r.Method != http.MethodPost || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			http.Error(w, "invalid runner lifecycle request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	case runnerMicroVMLifecyclePrefix + "run":
		g.handleRunHook(w, r)
	case runnerBootstrapPath:
		g.mu.RLock()
		receiver := g.receiver
		g.mu.RUnlock()
		if receiver == nil {
			http.Error(w, "runner MicroVM is not initialized", http.StatusServiceUnavailable)
			return
		}
		receiver.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (g *RunnerGuestRuntime) handleRunHook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.Fragment != "" || r.ContentLength > maxBootstrapBody ||
		len(r.TransferEncoding) > 0 || r.Body == nil {
		http.Error(w, "invalid runner lifecycle request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBootstrapBody+1))
	if err != nil || len(body) == 0 || len(body) > maxBootstrapBody {
		clear(body)
		http.Error(w, "invalid runner lifecycle request", http.StatusBadRequest)
		return
	}
	defer clear(body)
	var request runnerMicroVMRunHookRequest
	if decodeRunnerGuestJSON(body, &request) != nil || !runnerMicroVMIdentityPattern.MatchString(request.MicroVMID) ||
		strings.TrimSpace(request.RunHookPayload) != request.RunHookPayload || request.RunHookPayload == "" || len(request.RunHookPayload) > maxBootstrapBody {
		http.Error(w, "invalid runner lifecycle request", http.StatusBadRequest)
		return
	}
	var launch RunnerLaunchPayload
	if decodeRunnerGuestJSON([]byte(request.RunHookPayload), &launch) != nil || !validRunnerLaunchPayload(launch) {
		http.Error(w, "invalid runner lifecycle request", http.StatusBadRequest)
		return
	}
	receiver, err := NewRunnerBootstrapReceiver(g.publicKey, request.MicroVMID, boundRunnerBootstrapConsumer{launch: launch, next: g.consumer})
	if err != nil {
		http.Error(w, "invalid runner lifecycle request", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	if g.receiver != nil {
		if g.vmID != request.MicroVMID || !sameRunnerLaunchPayload(g.launch, launch) {
			g.mu.Unlock()
			http.Error(w, "runner MicroVM identity conflict", http.StatusConflict)
			return
		}
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	g.vmID, g.launch, g.receiver = request.MicroVMID, launch, receiver
	g.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func decodeRunnerGuestJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrRunnerBootstrap
	}
	return nil
}

func validRunnerLaunchPayload(payload RunnerLaunchPayload) bool {
	if payload.SchemaVersion != 1 || payload.WorkflowJobID <= 0 || payload.WorkflowRunID <= 0 || payload.RunAttempt <= 0 ||
		payload.RunnerName != "hako-"+strconv.FormatInt(payload.WorkflowJobID, 10)+"-"+strconv.FormatInt(payload.RunAttempt, 10) ||
		len(payload.Labels) == 0 || len(payload.Labels) > 100 {
		return false
	}
	repository, err := url.Parse(payload.RepositoryURL)
	if err != nil || repository.Scheme != "https" || repository.Host != "github.com" || repository.User != nil || repository.RawQuery != "" ||
		repository.ForceQuery || repository.Fragment != "" || repository.Opaque != "" || repository.RawPath != "" ||
		repository.Path != "/"+strings.TrimPrefix(repository.EscapedPath(), "/") ||
		strings.HasSuffix(repository.Path, ".git") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(repository.Path, "/"), "/")
	if len(parts) != 2 || !validRepositoryPart(parts[0]) || !validRepositoryPart(parts[1]) {
		return false
	}
	seen := make(map[string]struct{}, len(payload.Labels))
	for _, label := range payload.Labels {
		if label == "" || len(label) > 128 || strings.TrimSpace(label) != label || strings.ContainsAny(label, "\x00\r\n,") {
			return false
		}
		if _, exists := seen[label]; exists {
			return false
		}
		seen[label] = struct{}{}
	}
	return true
}

func sameRunnerLaunchPayload(left, right RunnerLaunchPayload) bool {
	return left.SchemaVersion == right.SchemaVersion && left.WorkflowJobID == right.WorkflowJobID && left.WorkflowRunID == right.WorkflowRunID &&
		left.RunAttempt == right.RunAttempt && left.RepositoryURL == right.RepositoryURL && left.RunnerName == right.RunnerName && slices.Equal(left.Labels, right.Labels)
}

type boundRunnerBootstrapConsumer struct {
	launch RunnerLaunchPayload
	next   RunnerBootstrapConsumer
}

func (c boundRunnerBootstrapConsumer) Consume(ctx context.Context, claims RunnerBootstrapClaims) error {
	if c.next == nil || claims.WorkflowJobID != c.launch.WorkflowJobID || claims.WorkflowRunID != c.launch.WorkflowRunID ||
		claims.RunAttempt != c.launch.RunAttempt || claims.RepositoryURL != c.launch.RepositoryURL || claims.RunnerName != c.launch.RunnerName ||
		!slices.Equal(claims.Labels, c.launch.Labels) {
		return ErrRunnerBootstrap
	}
	return c.next.Consume(ctx, claims)
}

var _ http.Handler = (*RunnerGuestRuntime)(nil)
var _ RunnerBootstrapConsumer = RunnerBootstrapConsumerFunc(nil)
