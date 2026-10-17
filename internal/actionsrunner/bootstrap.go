package actionsrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

const (
	runnerBootstrapPort = 8080
	runnerBootstrapPath = "/v1/actions-runner/bootstrap"
	maxBootstrapBody    = 16 << 10
)

var ErrRunnerBootstrap = errors.New("GitHub Actions runner bootstrap failed")

type RunnerBootstrapAuthAPI interface {
	CreateMicrovmAuthToken(context.Context, *lambdamicrovms.CreateMicrovmAuthTokenInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error)
}

type RunnerBootstrapClaims struct {
	SchemaVersion int      `json:"schema_version"`
	MicroVMID     string   `json:"microvm_id"`
	IntegrationID string   `json:"integration_id"`
	WorkflowJobID int64    `json:"workflow_job_id"`
	WorkflowRunID int64    `json:"workflow_run_id"`
	RunAttempt    int64    `json:"run_attempt"`
	RepositoryURL string   `json:"repository_url"`
	RunnerName    string   `json:"runner_name"`
	Labels        []string `json:"labels"`
	Token         string   `json:"registration_token"`
	ExpiresAt     int64    `json:"expires_at_unix"`
}

// SignedRunnerBootstrap is sent only to the guest's fixed bootstrap endpoint.
// Its String method prevents accidental credential disclosure in diagnostics.
type SignedRunnerBootstrap struct {
	Claims    RunnerBootstrapClaims `json:"claims"`
	Signature string                `json:"signature"`
}

func (SignedRunnerBootstrap) String() string   { return "[REDACTED signed runner bootstrap]" }
func (SignedRunnerBootstrap) GoString() string { return "[REDACTED signed runner bootstrap]" }

type RunnerBootstrapRequest struct {
	Instance   LambdaRunnerInstance
	Job        githubwebhook.ActionsRunnerJob
	Repository RunnerRepository
	Token      githubapp.RunnerRegistrationToken
}

type RunnerBootstrapper struct {
	api     RunnerBootstrapAuthAPI
	client  *http.Client
	private ed25519.PrivateKey
	clock   func() time.Time
}

func NewRunnerBootstrapper(api RunnerBootstrapAuthAPI, client *http.Client, privateKey ed25519.PrivateKey) (*RunnerBootstrapper, error) {
	if api == nil || client == nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrInvalidRunnerLaunch
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &RunnerBootstrapper{api: api, client: &copyClient, private: append(ed25519.PrivateKey(nil), privateKey...), clock: func() time.Time { return time.Now().UTC() }}, nil
}

// Deliver grants access to exactly the bootstrap port for five minutes, then
// posts a signed, job-bound envelope over HTTPS. The AWS proxy token and the
// GitHub runner token are never placed in URLs or RunHookPayload.
func (b *RunnerBootstrapper) Deliver(ctx context.Context, request RunnerBootstrapRequest) error {
	if b == nil || ctx == nil || validateBootstrapRequest(request, b.clock()) != nil {
		return ErrRunnerBootstrap
	}
	now := b.clock().UTC()
	authExpiryMinutes := int32(5)
	if request.Job.LeaseExpires.Sub(now) <= 5*time.Minute {
		authExpiryMinutes = 1
	}
	output, err := b.api.CreateMicrovmAuthToken(ctx, &lambdamicrovms.CreateMicrovmAuthTokenInput{
		MicrovmIdentifier:   aws.String(request.Instance.MicroVMID),
		AllowedPorts:        []types.PortSpecification{&types.PortSpecificationMemberPort{Value: runnerBootstrapPort}},
		ExpirationInMinutes: aws.Int32(authExpiryMinutes),
	})
	if err != nil || output == nil || strings.TrimSpace(output.AuthToken["X-aws-proxy-auth"]) == "" || len(output.AuthToken["X-aws-proxy-auth"]) > 4096 {
		return ErrRunnerBootstrap
	}
	claims := RunnerBootstrapClaims{SchemaVersion: 1, MicroVMID: request.Instance.MicroVMID, IntegrationID: request.Job.IntegrationID,
		WorkflowJobID: request.Job.JobID, WorkflowRunID: request.Job.WorkflowRunID, RunAttempt: request.Job.RunAttempt,
		RepositoryURL: "https://github.com/" + request.Repository.Owner + "/" + request.Repository.Name,
		RunnerName:    "hako-" + strconv.FormatInt(request.Job.JobID, 10) + "-" + strconv.FormatInt(request.Job.RunAttempt, 10),
		Labels:        append([]string(nil), request.Job.Labels...), Token: request.Token.Value(), ExpiresAt: request.Token.ExpiresAt().Unix()}
	if claims.ExpiresAt > now.Add(5*time.Minute).Unix() {
		claims.ExpiresAt = now.Add(5 * time.Minute).Unix()
	}
	if claims.ExpiresAt > request.Job.LeaseExpires.Unix() {
		claims.ExpiresAt = request.Job.LeaseExpires.Unix()
	}
	if claims.ExpiresAt <= now.Unix() {
		return ErrRunnerBootstrap
	}
	envelope, err := SignRunnerBootstrap(b.private, claims)
	if err != nil {
		return ErrRunnerBootstrap
	}
	body, err := json.Marshal(envelope)
	if err != nil || len(body) > maxBootstrapBody {
		return ErrRunnerBootstrap
	}
	endpoint, err := runnerBootstrapEndpoint(request.Instance)
	if err != nil {
		return ErrRunnerBootstrap
	}
	postCtx, cancel := context.WithDeadline(ctx, time.Unix(claims.ExpiresAt, 0))
	defer cancel()
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrRunnerBootstrap
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("X-aws-proxy-auth", output.AuthToken["X-aws-proxy-auth"])
	resp, err := b.client.Do(req)
	if err != nil {
		return ErrRunnerBootstrap
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || resp.ContentLength > 1024 || resp.Header.Get("Cache-Control") != "no-store" {
		return ErrRunnerBootstrap
	}
	response, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil || len(response) > 1024 {
		return ErrRunnerBootstrap
	}
	return nil
}

func VerifyRunnerBootstrap(publicKey ed25519.PublicKey, envelope SignedRunnerBootstrap, microVMID string, now time.Time) error {
	if len(publicKey) != ed25519.PublicKeySize || envelope.Claims.SchemaVersion != 1 || envelope.Claims.MicroVMID != microVMID ||
		envelope.Claims.IntegrationID == "" || envelope.Claims.WorkflowJobID <= 0 || envelope.Claims.WorkflowRunID <= 0 || envelope.Claims.RunAttempt <= 0 ||
		envelope.Claims.Token == "" || len(envelope.Claims.Token) > 4096 || envelope.Claims.ExpiresAt <= now.UTC().Unix() ||
		envelope.Claims.ExpiresAt > now.UTC().Add(5*time.Minute).Unix() {
		return ErrRunnerBootstrap
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ErrRunnerBootstrap
	}
	encodedClaims, err := json.Marshal(envelope.Claims)
	if err != nil || !ed25519.Verify(publicKey, encodedClaims, signature) {
		return ErrRunnerBootstrap
	}
	return nil
}

func SignRunnerBootstrap(privateKey ed25519.PrivateKey, claims RunnerBootstrapClaims) (SignedRunnerBootstrap, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return SignedRunnerBootstrap{}, ErrRunnerBootstrap
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		return SignedRunnerBootstrap{}, ErrRunnerBootstrap
	}
	return SignedRunnerBootstrap{Claims: claims, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, encoded))}, nil
}

func runnerBootstrapEndpoint(instance LambdaRunnerInstance) (string, error) {
	parsed, err := url.Parse(instance.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" ||
		parsed.Path != "" && parsed.Path != "/" || parsed.Hostname() != instance.MicroVMID+".lambda-microvm."+runnerRegion(instance.ImageIdentifier)+".on.aws" {
		return "", ErrRunnerBootstrap
	}
	parsed.Path = runnerBootstrapPath
	return parsed.String(), nil
}

func runnerRegion(imageARN string) string {
	match := arnPattern.FindStringSubmatch(imageARN)
	if len(match) != 5 {
		return ""
	}
	return match[1]
}

func validateBootstrapRequest(request RunnerBootstrapRequest, now time.Time) error {
	if validateRunnerLaunchJob(request.Job) != nil || request.Job.State != "starting" || !request.Job.LeaseExpires.After(now) ||
		!validRepositoryPart(request.Repository.Owner) || !validRepositoryPart(request.Repository.Name) ||
		request.Instance.MicroVMID == "" || request.Instance.ResourcePlaneID == "" || !request.Token.ExpiresAt().After(now.Add(30*time.Second)) ||
		request.Token.Value() == "" || len(request.Token.Value()) > 4096 {
		return ErrRunnerBootstrap
	}
	_, err := runnerBootstrapEndpoint(request.Instance)
	return err
}

// BootstrapDigest returns a stable digest for audit correlation without
// exposing the signed body or either credential.
func BootstrapDigest(envelope SignedRunnerBootstrap) string {
	body, _ := json.Marshal(envelope.Claims)
	digest := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
