package actionsrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunnerGuestRuntimeBindsRunHookBeforeOneMatchingBootstrap(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	launch := RunnerLaunchPayload{SchemaVersion: 1, WorkflowJobID: 42, WorkflowRunID: 43, RunAttempt: 2,
		RepositoryURL: "https://github.com/acme/api", RunnerName: "hako-42-2", Labels: []string{"self-hosted", "hako", "arm64"}}
	consumed := 0
	var consumedKey []byte
	runtime, err := NewRunnerGuestRuntime(publicKey, RunnerBootstrapConsumerFunc(func(_ context.Context, claims RunnerBootstrapClaims) error {
		consumed++
		if claims.WorkflowJobID != launch.WorkflowJobID || claims.RepositoryURL != launch.RepositoryURL || string(claims.ModelCredential) != "sk-ant-synthetic-secret" {
			t.Fatalf("unexpected Action bootstrap claims: %s", claims)
		}
		consumedKey = claims.ModelCredential
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	claims := RunnerBootstrapClaims{SchemaVersion: 1, MicroVMID: "mvm-guest-1", IntegrationID: "integration_runner", WorkflowJobID: 42,
		WorkflowRunID: 43, RunAttempt: 2, RepositoryURL: launch.RepositoryURL, RunnerName: launch.RunnerName, Labels: launch.Labels,
		Token: "synthetic-registration-token", ModelCredential: []byte("sk-ant-synthetic-secret"), ExpiresAt: now.Add(time.Minute).Unix()}
	envelope, err := SignRunnerBootstrap(privateKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendGuestBootstrap(runtime, envelope); got.Code != http.StatusServiceUnavailable || consumed != 0 {
		t.Fatalf("bootstrap accepted before /run: status=%d consumed=%d", got.Code, consumed)
	}
	if got := sendRunnerRunHook(runtime, "mvm-guest-1", launch); got.Code != http.StatusOK {
		t.Fatalf("run hook bind status=%d body=%s", got.Code, got.Body.String())
	}
	if got := sendRunnerRunHook(runtime, "mvm-guest-1", launch); got.Code != http.StatusOK {
		t.Fatalf("identical /run retry was not idempotent: %d", got.Code)
	}
	conflicting := launch
	conflicting.WorkflowJobID++
	conflicting.RunnerName = "hako-43-2"
	if got := sendRunnerRunHook(runtime, "mvm-guest-1", conflicting); got.Code != http.StatusConflict {
		t.Fatalf("conflicting /run identity status=%d", got.Code)
	}
	if got := sendGuestBootstrap(runtime, envelope); got.Code != http.StatusAccepted || consumed != 1 {
		t.Fatalf("matching bootstrap status=%d consumed=%d", got.Code, consumed)
	}
	if !bytes.Equal(consumedKey, make([]byte, len(consumedKey))) {
		t.Fatal("guest runtime retained model credential bytes after consumer returned")
	}
	if got := sendGuestBootstrap(runtime, envelope); got.Code != http.StatusConflict || consumed != 1 {
		t.Fatalf("bootstrap replay status=%d consumed=%d", got.Code, consumed)
	}
}

func TestRunnerGuestRuntimeRejectsWrongJobAndInvalidRunHook(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	consumed := 0
	runtime, err := NewRunnerGuestRuntime(publicKey, RunnerBootstrapConsumerFunc(func(context.Context, RunnerBootstrapClaims) error {
		consumed++
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	launch := RunnerLaunchPayload{SchemaVersion: 1, WorkflowJobID: 42, WorkflowRunID: 43, RunAttempt: 1,
		RepositoryURL: "https://github.com/acme/api", RunnerName: "hako-42-1", Labels: []string{"self-hosted", "hako"}}
	bad := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, runnerMicroVMLifecyclePrefix+"run", strings.NewReader(`{"microvmId":"mvm-1","runHookPayload":"{}","unknown":true}`))
	bad.Header().Set("Content-Type", "application/json")
	runtime.ServeHTTP(bad, request)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown run-hook field accepted: %d", bad.Code)
	}
	if got := sendRunnerRunHook(runtime, "mvm-1", launch); got.Code != http.StatusOK {
		t.Fatalf("valid run hook status=%d", got.Code)
	}
	claims := RunnerBootstrapClaims{SchemaVersion: 1, MicroVMID: "mvm-1", IntegrationID: "integration_runner", WorkflowJobID: 99,
		WorkflowRunID: 43, RunAttempt: 1, RepositoryURL: launch.RepositoryURL, RunnerName: "hako-99-1", Labels: launch.Labels,
		Token: "synthetic-registration-token", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	envelope, err := SignRunnerBootstrap(privateKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendGuestBootstrap(runtime, envelope); got.Code != http.StatusInternalServerError || consumed != 0 {
		t.Fatalf("wrong job reached runner consumer: status=%d consumed=%d", got.Code, consumed)
	}
	if got := sendGuestBootstrap(runtime, envelope); got.Code != http.StatusConflict || consumed != 0 {
		t.Fatalf("failed wrong-job envelope was retried: status=%d consumed=%d", got.Code, consumed)
	}
}

func TestValidRunnerLaunchPayloadRejectsForeignOrAmbiguousIdentity(t *testing.T) {
	base := RunnerLaunchPayload{SchemaVersion: 1, WorkflowJobID: 42, WorkflowRunID: 43, RunAttempt: 1,
		RepositoryURL: "https://github.com/acme/api", RunnerName: "hako-42-1", Labels: []string{"self-hosted", "hako"}}
	if !validRunnerLaunchPayload(base) {
		t.Fatal("valid provider launch payload rejected")
	}
	cases := map[string]func(*RunnerLaunchPayload){
		"foreign host":       func(p *RunnerLaunchPayload) { p.RepositoryURL = "https://github.example/acme/api" },
		"encoded path":       func(p *RunnerLaunchPayload) { p.RepositoryURL = "https://github.com/acme%2Fother/api" },
		"query":              func(p *RunnerLaunchPayload) { p.RepositoryURL = "https://github.com/acme/api?redirect=other" },
		"mismatched name":    func(p *RunnerLaunchPayload) { p.RunnerName = "attacker-runner" },
		"duplicate labels":   func(p *RunnerLaunchPayload) { p.Labels = []string{"hako", "hako"} },
		"command-like label": func(p *RunnerLaunchPayload) { p.Labels = []string{"hako,privileged"} },
		"invalid job id":     func(p *RunnerLaunchPayload) { p.WorkflowJobID = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			payload := base
			payload.Labels = append([]string(nil), base.Labels...)
			mutate(&payload)
			if validRunnerLaunchPayload(payload) {
				t.Fatalf("unsafe Run Hook payload accepted: %s", name)
			}
		})
	}
}

func sendRunnerRunHook(handler http.Handler, microVMID string, payload RunnerLaunchPayload) *httptest.ResponseRecorder {
	inner, _ := json.Marshal(payload)
	defer clear(inner)
	outer, _ := json.Marshal(runnerMicroVMRunHookRequest{MicroVMID: microVMID, RunHookPayload: string(inner)})
	defer clear(outer)
	request := httptest.NewRequest(http.MethodPost, runnerMicroVMLifecyclePrefix+"run", bytes.NewReader(outer))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func sendGuestBootstrap(handler http.Handler, envelope SignedRunnerBootstrap) *httptest.ResponseRecorder {
	body, _ := json.Marshal(envelope)
	defer clear(body)
	request := httptest.NewRequest(http.MethodPost, runnerBootstrapPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
