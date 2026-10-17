package actionsrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/githubapp"
)

type runnerBootstrapAuthStub struct {
	input  *lambdamicrovms.CreateMicrovmAuthTokenInput
	output *lambdamicrovms.CreateMicrovmAuthTokenOutput
	err    error
}

func (s *runnerBootstrapAuthStub) CreateMicrovmAuthToken(_ context.Context, input *lambdamicrovms.CreateMicrovmAuthTokenInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	s.input = input
	return s.output, s.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRunnerBootstrapperUsesScopedAWSAuthAndSignedJobEnvelope(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	api := &runnerBootstrapAuthStub{output: &lambdamicrovms.CreateMicrovmAuthTokenOutput{AuthToken: map[string]string{"X-aws-proxy-auth": "aws-proxy-secret"}}}
	var requestSeen bool
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestSeen = true
		if req.Method != http.MethodPost || req.URL.String() != "https://runner-microvm-1.lambda-microvm.ap-northeast-1.on.aws"+runnerBootstrapPath ||
			req.Header.Get("X-aws-proxy-auth") != "aws-proxy-secret" || req.Header.Get("Cache-Control") != "no-store" || req.URL.RawQuery != "" {
			t.Errorf("unsafe bootstrap request: method=%s url=%s headers=%v", req.Method, req.URL, req.Header)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "aws-proxy-secret") {
			t.Fatal("AWS proxy credential was copied into the body")
		}
		var envelope SignedRunnerBootstrap
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatal(err)
		}
		if err := VerifyRunnerBootstrap(publicKey, envelope, "runner-microvm-1", now); err != nil {
			t.Fatalf("guest rejected signed bootstrap: %v", err)
		}
		if envelope.Claims.IntegrationID != "integration_runner" || envelope.Claims.WorkflowJobID != 9901 || envelope.Claims.WorkflowRunID != 9902 ||
			envelope.Claims.RunAttempt != 1 || envelope.Claims.RepositoryURL != "https://github.com/acme/api" || envelope.Claims.RunnerName != "hako-9901-1" ||
			envelope.Claims.Token != "synthetic-registration-token" || strings.Join(envelope.Claims.Labels, ",") != "self-hosted,hako,standard" ||
			string(envelope.Claims.ModelCredential) != "sk-ant-synthetic-secret" {
			t.Fatalf("bootstrap lost exact admitted job identity: %+v", envelope.Claims)
		}
		if strings.Contains(fmt.Sprint(envelope), "sk-ant-synthetic-secret") || strings.Contains(fmt.Sprintf("%#v", envelope.Claims), "sk-ant-synthetic-secret") {
			t.Fatal("signed bootstrap diagnostics exposed the model credential")
		}
		return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"Cache-Control": []string{"no-store"}}, Body: io.NopCloser(strings.NewReader(`{}`)), ContentLength: 2, Request: req}, nil
	})}
	bootstrapper, err := NewRunnerBootstrapper(api, client, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapper.clock = func() time.Time { return now }
	token, err := githubapp.NewRunnerRegistrationToken("synthetic-registration-token", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	instance := LambdaRunnerInstance{ResourcePlaneID: "rp-tokyo-01", MicroVMID: "runner-microvm-1",
		Endpoint: "https://runner-microvm-1.lambda-microvm.ap-northeast-1.on.aws", ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:runner"}
	if err := bootstrapper.Deliver(context.Background(), RunnerBootstrapRequest{Instance: instance, Job: lambdaRunnerJob(), Repository: RunnerRepository{Owner: "acme", Name: "api"}, Token: token,
		ModelCredential: []byte("sk-ant-synthetic-secret")}); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if !requestSeen || api.input == nil || aws.ToString(api.input.MicrovmIdentifier) != instance.MicroVMID || aws.ToInt32(api.input.ExpirationInMinutes) != 1 ||
		len(api.input.AllowedPorts) != 1 || api.input.AllowedPorts[0].(*types.PortSpecificationMemberPort).Value != runnerBootstrapPort {
		t.Fatalf("bootstrap did not receive exact port-scoped five-minute auth: seen=%v input=%+v", requestSeen, api.input)
	}
}

func TestRunnerBootstrapRejectsOversizedModelCredential(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	claims := RunnerBootstrapClaims{SchemaVersion: 1, MicroVMID: "runner-microvm-1", IntegrationID: "integration_runner", WorkflowJobID: 9901,
		WorkflowRunID: 9902, RunAttempt: 1, Token: "synthetic-registration-token", ExpiresAt: time.Now().Add(time.Minute).Unix(), ModelCredential: make([]byte, 8193)}
	if _, err := SignRunnerBootstrap(privateKey, claims); err == nil {
		t.Fatal("oversized model credential was signed")
	}
	clear(claims.ModelCredential)
}

func TestRunnerBootstrapVerificationRejectsTamperingAndWrongVM(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	envelope, err := SignRunnerBootstrap(privateKey, RunnerBootstrapClaims{SchemaVersion: 1, MicroVMID: "runner-microvm-1", IntegrationID: "integration_runner",
		WorkflowJobID: 9901, WorkflowRunID: 9902, RunAttempt: 1, RepositoryURL: "https://github.com/acme/api", RunnerName: "hako-9901-1",
		Labels: []string{"self-hosted", "hako"}, Token: "synthetic-registration-token", ExpiresAt: now.Add(2 * time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRunnerBootstrap(publicKey, envelope, "runner-microvm-1", now); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	tampered := envelope
	tampered.Claims.Token = "attacker-token"
	if err := VerifyRunnerBootstrap(publicKey, tampered, "runner-microvm-1", now); err == nil {
		t.Fatal("modified token passed signature verification")
	}
	if err := VerifyRunnerBootstrap(publicKey, envelope, "other-microvm", now); err == nil {
		t.Fatal("bootstrap for another MicroVM was accepted")
	}
	if err := VerifyRunnerBootstrap(publicKey, envelope, "runner-microvm-1", now.Add(3*time.Minute)); err == nil {
		t.Fatal("expired bootstrap was accepted")
	}
}

func TestRunnerBootstrapperRejectsUnsafeEndpointBeforeTokenRequest(t *testing.T) {
	api := &runnerBootstrapAuthStub{output: &lambdamicrovms.CreateMicrovmAuthTokenOutput{AuthToken: map[string]string{"X-aws-proxy-auth": "proxy"}}}
	bootstrapper, err := NewRunnerBootstrapper(api, &http.Client{}, make(ed25519.PrivateKey, ed25519.PrivateKeySize))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := githubapp.NewRunnerRegistrationToken("synthetic-registration-token", time.Now().Add(time.Hour))
	instance := LambdaRunnerInstance{ResourcePlaneID: "rp-tokyo-01", MicroVMID: "runner-microvm-1", Endpoint: "https://attacker.invalid",
		ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:runner"}
	if err := bootstrapper.Deliver(context.Background(), RunnerBootstrapRequest{Instance: instance, Job: lambdaRunnerJob(), Repository: RunnerRepository{Owner: "acme", Name: "api"}, Token: token}); err == nil {
		t.Fatal("untrusted guest endpoint was accepted")
	}
	if api.input != nil {
		t.Fatal("unsafe endpoint caused AWS authorization token issuance")
	}
}
