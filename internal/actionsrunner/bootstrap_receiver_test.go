package actionsrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunnerBootstrapReceiverVerifiesVMAndConsumesOnlyOnce(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	consumed := 0
	var credentialBuffer []byte
	receiver, err := NewRunnerBootstrapReceiver(publicKey, "runner-microvm-1", RunnerBootstrapConsumerFunc(func(_ context.Context, claims RunnerBootstrapClaims) error {
		consumed++
		if claims.MicroVMID != "runner-microvm-1" || claims.WorkflowJobID != 42 || string(claims.ModelCredential) != "sk-ant-synthetic-secret" {
			t.Fatalf("guest received wrong bootstrap claims: %s", claims)
		}
		credentialBuffer = claims.ModelCredential
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	receiver.clock = func() time.Time { return now }
	claims := RunnerBootstrapClaims{SchemaVersion: 1, MicroVMID: "runner-microvm-1", IntegrationID: "integration_runner", WorkflowJobID: 42,
		WorkflowRunID: 43, RunAttempt: 1, RepositoryURL: "https://github.com/acme/api", RunnerName: "hako-42-1", Labels: []string{"self-hosted", "hako"},
		Token: "synthetic-registration-token", ModelCredential: []byte("sk-ant-synthetic-secret"), ExpiresAt: now.Add(time.Minute).Unix()}
	valid, err := SignRunnerBootstrap(privateKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	wrongVMClaims := claims
	wrongVMClaims.MicroVMID = "runner-microvm-2"
	wrongVM, err := SignRunnerBootstrap(privateKey, wrongVMClaims)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendRunnerBootstrap(receiver, wrongVM); got.Code != http.StatusForbidden || consumed != 0 {
		t.Fatalf("wrong VM accepted: status=%d consumed=%d", got.Code, consumed)
	}
	if got := sendRunnerBootstrap(receiver, valid); got.Code != http.StatusAccepted || got.Header().Get("Cache-Control") != "no-store" || consumed != 1 {
		t.Fatalf("valid bootstrap failed: status=%d consumed=%d headers=%v", got.Code, consumed, got.Header())
	}
	if !bytes.Equal(credentialBuffer, make([]byte, len(credentialBuffer))) {
		t.Fatal("guest receiver retained the credential buffer after consumer returned")
	}
	if got := sendRunnerBootstrap(receiver, valid); got.Code != http.StatusConflict || consumed != 1 {
		t.Fatalf("bootstrap replay was not rejected: status=%d consumed=%d", got.Code, consumed)
	}
}

func TestRunnerBootstrapReceiverConsumesFailedDeliveryAndRejectsMalformedEnvelope(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	consumed := 0
	receiver, err := NewRunnerBootstrapReceiver(publicKey, "runner-microvm-1", RunnerBootstrapConsumerFunc(func(context.Context, RunnerBootstrapClaims) error {
		consumed++
		return errors.New("synthetic runner startup failure")
	}))
	if err != nil {
		t.Fatal(err)
	}
	receiver.clock = func() time.Time { return now }
	claims := RunnerBootstrapClaims{SchemaVersion: 1, MicroVMID: "runner-microvm-1", IntegrationID: "integration_runner", WorkflowJobID: 42,
		WorkflowRunID: 43, RunAttempt: 1, Token: "synthetic-registration-token", ModelCredential: []byte("sk-ant-synthetic-secret"), ExpiresAt: now.Add(time.Minute).Unix()}
	envelope, err := SignRunnerBootstrap(privateKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	malformed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, runnerBootstrapPath, strings.NewReader(`{"unknown":true}`))
	req.Header.Set("Content-Type", "application/json")
	receiver.ServeHTTP(malformed, req)
	if malformed.Code != http.StatusBadRequest || consumed != 0 {
		t.Fatalf("malformed envelope accepted: status=%d consumed=%d", malformed.Code, consumed)
	}
	if got := sendRunnerBootstrap(receiver, envelope); got.Code != http.StatusInternalServerError || consumed != 1 {
		t.Fatalf("consumer failure status=%d consumed=%d", got.Code, consumed)
	}
	if got := sendRunnerBootstrap(receiver, envelope); got.Code != http.StatusConflict || consumed != 1 {
		t.Fatalf("failed bootstrap was retried in guest: status=%d consumed=%d", got.Code, consumed)
	}
}

func sendRunnerBootstrap(receiver http.Handler, envelope SignedRunnerBootstrap) *httptest.ResponseRecorder {
	body, _ := json.Marshal(envelope)
	defer clear(body)
	request := httptest.NewRequest(http.MethodPost, runnerBootstrapPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	return response
}
