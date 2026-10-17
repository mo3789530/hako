package actionsrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"
)

var runnerMicroVMIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)

// RunnerBootstrapConsumer synchronously installs one verified bootstrap in the
// guest runner process. It must not retain ModelCredential; the receiver wipes
// that buffer as soon as Consume returns.
type RunnerBootstrapConsumer interface {
	Consume(context.Context, RunnerBootstrapClaims) error
}

// RunnerBootstrapConsumerFunc adapts a function to RunnerBootstrapConsumer.
type RunnerBootstrapConsumerFunc func(context.Context, RunnerBootstrapClaims) error

func (f RunnerBootstrapConsumerFunc) Consume(ctx context.Context, claims RunnerBootstrapClaims) error {
	return f(ctx, claims)
}

// RunnerBootstrapReceiver is a single-use guest endpoint. The receiver marks
// itself consumed before invoking the consumer, so retries can never start a
// second runner in the same MicroVM after an ambiguous callback result.
type RunnerBootstrapReceiver struct {
	publicKey ed25519.PublicKey
	microVMID string
	consumer  RunnerBootstrapConsumer
	clock     func() time.Time

	mu       sync.Mutex
	consumed bool
}

func NewRunnerBootstrapReceiver(publicKey ed25519.PublicKey, microVMID string, consumer RunnerBootstrapConsumer) (*RunnerBootstrapReceiver, error) {
	if len(publicKey) != ed25519.PublicKeySize || !runnerMicroVMIdentityPattern.MatchString(microVMID) || consumer == nil {
		return nil, ErrRunnerBootstrap
	}
	return &RunnerBootstrapReceiver{publicKey: append(ed25519.PublicKey(nil), publicKey...), microVMID: microVMID,
		consumer: consumer, clock: func() time.Time { return time.Now().UTC() }}, nil
}

func (r *RunnerBootstrapReceiver) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r == nil || request == nil || request.Method != http.MethodPost || request.URL == nil || request.URL.Path != runnerBootstrapPath ||
		request.URL.RawQuery != "" || request.Body == nil || request.ContentLength > maxBootstrapBody ||
		request.Header.Get("Content-Type") != "application/json" {
		writeRunnerBootstrapReceiverError(w, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBootstrapBody+1))
	if err != nil || len(body) == 0 || len(body) > maxBootstrapBody {
		clear(body)
		writeRunnerBootstrapReceiverError(w, http.StatusBadRequest)
		return
	}
	defer clear(body)
	var envelope SignedRunnerBootstrap
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil {
		writeRunnerBootstrapReceiverError(w, http.StatusBadRequest)
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || VerifyRunnerBootstrap(r.publicKey, envelope, r.microVMID, r.clock().UTC()) != nil {
		clear(envelope.Claims.ModelCredential)
		writeRunnerBootstrapReceiverError(w, http.StatusForbidden)
		return
	}

	r.mu.Lock()
	if r.consumed {
		r.mu.Unlock()
		clear(envelope.Claims.ModelCredential)
		writeRunnerBootstrapReceiverError(w, http.StatusConflict)
		return
	}
	r.consumed = true
	r.mu.Unlock()

	err = r.consumer.Consume(request.Context(), envelope.Claims)
	clear(envelope.Claims.ModelCredential)
	if err != nil {
		writeRunnerBootstrapReceiverError(w, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func writeRunnerBootstrapReceiverError(w http.ResponseWriter, status int) {
	http.Error(w, http.StatusText(status), status)
}
