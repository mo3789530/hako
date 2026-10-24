package workspaceruntime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

const (
	workspaceServiceAgentPort     int32 = 8080
	workspaceServiceTokenMinutes  int32 = 2
	maxWorkspaceServiceReplyBytes       = 8 << 10
)

var ErrWorkspaceServiceDelivery = errors.New("Workspace Service delivery failed")

var workspaceServiceRegionPattern = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z0-9-]+-[0-9]$`)

type serviceMicroVMAPI interface {
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	CreateMicrovmAuthToken(context.Context, *lambdamicrovms.CreateMicrovmAuthTokenInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error)
}

type serviceCapabilityIssuer interface {
	Issue(domain.WorkspaceServiceCommand, string) (string, workspaceagent.ServiceCapability, error)
}

type workspaceServiceObservation struct {
	ServiceID        domain.ServiceID `json:"service_id"`
	Revision         int64            `json:"revision"`
	ObservedRevision int64            `json:"observed_revision"`
	DesiredState     string           `json:"desired_state"`
	ObservedState    string           `json:"observed_state"`
	Healthy          bool             `json:"healthy"`
	ErrorCode        string           `json:"error_code,omitempty"`
}

type WorkspaceServiceRuntimeReport struct {
	TenantID             domain.TenantID        `json:"tenant_id"`
	WorkspaceID          domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID      domain.ResourcePlaneID `json:"resource_plane_id"`
	MicroVMID            string                 `json:"microvm_id"`
	AllocationGeneration int64                  `json:"allocation_generation"`
	ServiceID            domain.ServiceID       `json:"service_id"`
	Revision             int64                  `json:"revision"`
	ObservedRevision     int64                  `json:"observed_revision"`
	DesiredState         string                 `json:"desired_state"`
	ObservedState        string                 `json:"observed_state"`
	Healthy              bool                   `json:"healthy"`
	ErrorCode            string                 `json:"error_code,omitempty"`
	ObservedAt           time.Time              `json:"observed_at"`
}

type WorkspaceServiceGuest interface {
	Reconcile(context.Context, string, string, string, domain.WorkspaceServiceCommand, map[string]string) (workspaceServiceObservation, error)
}

type WorkspaceServiceReportSink interface {
	Report(context.Context, WorkspaceServiceRuntimeReport) error
}

type WorkspaceServiceCommandResolver interface {
	Resolve(context.Context, domain.WorkspaceServiceCommand) (domain.WorkspaceServiceCommand, error)
}

type WorkspaceServiceCommandRuntime interface {
	Handle(context.Context, []byte) error
}

// WorkspaceServiceCommandConsumer resolves the currently journaled Lambda
// MicroVM, mints credentials scoped to that one VM and port 8080, and reports
// the Guest's revision-fenced result. It never reads Control Plane storage.
type WorkspaceServiceCommandConsumer struct {
	planeID  domain.ResourcePlaneID
	region   string
	store    workspacejournal.Store
	api      serviceMicroVMAPI
	issuer   serviceCapabilityIssuer
	guest    WorkspaceServiceGuest
	reports  WorkspaceServiceReportSink
	resolver WorkspaceServiceCommandResolver
	secrets  WorkspaceServiceSecretProvider
	now      func() time.Time
}

func NewWorkspaceServiceCommandConsumer(planeID domain.ResourcePlaneID, region string, store workspacejournal.Store,
	api serviceMicroVMAPI, issuer serviceCapabilityIssuer, guest WorkspaceServiceGuest, reports WorkspaceServiceReportSink,
	resolver WorkspaceServiceCommandResolver, now func() time.Time) (*WorkspaceServiceCommandConsumer, error) {
	if planeID == "" || !workspaceServiceRegionPattern.MatchString(region) || store == nil || api == nil || issuer == nil || guest == nil || reports == nil || resolver == nil {
		return nil, errors.New("Workspace Service consumer requires Plane identity, journal, Lambda API, capability issuer, Guest, report sink, and desired-state resolver")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &WorkspaceServiceCommandConsumer{planeID: planeID, region: region, store: store, api: api, issuer: issuer,
		guest: guest, reports: reports, resolver: resolver, now: now}, nil
}

// WithSecretProvider attaches the Resource Plane-only generated credential
// provider. It is optional for templates without generated credentials.
func (c *WorkspaceServiceCommandConsumer) WithSecretProvider(provider WorkspaceServiceSecretProvider) *WorkspaceServiceCommandConsumer {
	if c != nil {
		c.secrets = provider
	}
	return c
}

func (c *WorkspaceServiceCommandConsumer) Handle(ctx context.Context, payload []byte) error {
	if c == nil || ctx == nil || len(payload) == 0 || len(payload) > 64<<10 {
		return ErrWorkspaceServiceDelivery
	}
	var command domain.WorkspaceServiceCommand
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&command) != nil || decoder.Decode(new(any)) != io.EOF || !validWorkspaceServiceCommand(command) || command.ResourcePlaneID != c.planeID {
		return ErrWorkspaceServiceDelivery
	}
	current, err := c.resolver.Resolve(ctx, command)
	if err != nil || !validWorkspaceServiceCommand(current) || current.ResourcePlaneID != c.planeID ||
		current.TenantID != command.TenantID || current.WorkspaceID != command.WorkspaceID || current.ServiceID != command.ServiceID ||
		current.Revision < command.Revision {
		return ErrWorkspaceServiceDelivery
	}
	command = current
	snapshot, err := c.store.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if err != nil || snapshot.ETag == "" || snapshot.Journal.Validate() != nil || snapshot.Journal.TenantID != command.TenantID ||
		snapshot.Journal.WorkspaceID != command.WorkspaceID || snapshot.Journal.ResourcePlaneID != command.ResourcePlaneID ||
		snapshot.Journal.State != workspacejournal.Running || snapshot.Journal.ProviderID == "" {
		return ErrWorkspaceServiceDelivery
	}
	vmID := snapshot.Journal.ProviderID
	vm, err := c.api.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(vmID)})
	if err != nil || vm == nil || vm.State != types.MicrovmStateRunning || aws.ToString(vm.MicrovmId) != vmID || verifyMicroVM(snapshot.Journal, vm) != nil {
		return ErrWorkspaceServiceDelivery
	}
	endpoint := aws.ToString(vm.Endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, strings.ToLower(vmID+".lambda-microvm."+c.region+".on.aws")) ||
		parsed.User != nil || parsed.Port() != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrWorkspaceServiceDelivery
	}
	tokenOutput, err := c.api.CreateMicrovmAuthToken(ctx, &lambdamicrovms.CreateMicrovmAuthTokenInput{
		MicrovmIdentifier: aws.String(vmID), AllowedPorts: []types.PortSpecification{&types.PortSpecificationMemberPort{Value: workspaceServiceAgentPort}},
		ExpirationInMinutes: aws.Int32(workspaceServiceTokenMinutes),
	})
	if err != nil || tokenOutput == nil {
		return ErrWorkspaceServiceDelivery
	}
	authToken := tokenOutput.AuthToken["X-aws-proxy-auth"]
	if authToken == "" || len(authToken) > 8000 || strings.ContainsAny(authToken, " \t\r\n") {
		return ErrWorkspaceServiceDelivery
	}
	capability, _, err := c.issuer.Issue(command, vmID)
	if err != nil || capability == "" || strings.ContainsAny(capability, " \t\r\n") {
		return ErrWorkspaceServiceDelivery
	}
	generatedSecrets := map[string]string{}
	template, err := servicecatalog.Decode(bytes.NewReader(command.Config))
	if err != nil {
		return ErrWorkspaceServiceDelivery
	}
	if command.DesiredState == "running" && len(template.GeneratedSecrets) != 0 {
		if c.secrets == nil {
			return ErrWorkspaceServiceDelivery
		}
		generatedSecrets, err = c.secrets.Resolve(ctx, command)
		if err != nil || !validWorkspaceServiceGeneratedSecrets(template, generatedSecrets) {
			return ErrWorkspaceServiceDelivery
		}
	}
	observation, guestErr := c.guest.Reconcile(ctx, endpoint, authToken, capability, command, generatedSecrets)
	if !validWorkspaceServiceObservation(command, observation) {
		return ErrWorkspaceServiceDelivery
	}
	// Desired state can change while the Guest is applying a revision. Do not
	// acknowledge that queue delivery or publish its observation as current;
	// return a retryable error so the same wake-up resolves and applies the new
	// authoritative revision. This narrows the race to the Guest side effect and
	// prevents a stale success report from hiding an unprocessed stop/remove.
	latest, err := c.resolver.Resolve(ctx, command)
	if err != nil || !validWorkspaceServiceCommand(latest) || latest.ResourcePlaneID != c.planeID ||
		latest.TenantID != command.TenantID || latest.WorkspaceID != command.WorkspaceID || latest.ServiceID != command.ServiceID ||
		latest.Revision < command.Revision {
		return ErrWorkspaceServiceDelivery
	}
	if !sameWorkspaceServiceDesiredState(latest, command) {
		return ErrWorkspaceServiceDelivery
	}
	// The journal is the Resource Plane's authority for the active allocation.
	// A Guest side effect is only reportable while it still belongs to that exact
	// allocation; a replaced MicroVM must never confirm cleanup for its successor.
	latestSnapshot, err := c.store.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if err != nil || latestSnapshot.ETag == "" || latestSnapshot.Journal.Validate() != nil || latestSnapshot.Journal.State != workspacejournal.Running ||
		latestSnapshot.Journal.ProviderID != vmID || latestSnapshot.Journal.AllocationGeneration != snapshot.Journal.AllocationGeneration {
		return ErrWorkspaceServiceDelivery
	}
	if guestErr == nil && observation.ObservedState == "deleted" && len(template.GeneratedSecrets) != 0 {
		retirer, ok := c.secrets.(WorkspaceServiceSecretRetirer)
		if !ok || retirer.Retire(ctx, command) != nil {
			return ErrWorkspaceServiceDelivery
		}
	}
	if guestErr == nil && command.DesiredState == "running" && len(template.GeneratedSecrets) != 0 && workspaceServiceCredentialGeneration(command) > 1 {
		if observation.ObservedState != "healthy" || !observation.Healthy {
			return ErrWorkspaceServiceDelivery
		}
		committer, ok := c.secrets.(WorkspaceServiceSecretCommitter)
		if !ok || committer.CommitRotation(ctx, command) != nil {
			return ErrWorkspaceServiceDelivery
		}
	}
	report := WorkspaceServiceRuntimeReport{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, MicroVMID: vmID, AllocationGeneration: snapshot.Journal.AllocationGeneration,
		ServiceID: command.ServiceID, Revision: observation.Revision,
		ObservedRevision: observation.ObservedRevision, DesiredState: observation.DesiredState, ObservedState: observation.ObservedState,
		Healthy: observation.Healthy, ErrorCode: observation.ErrorCode, ObservedAt: c.now().UTC()}
	if err := c.reports.Report(ctx, report); err != nil {
		return fmt.Errorf("report Workspace Service result: %w", err)
	}
	if guestErr != nil {
		return fmt.Errorf("%w: %v", ErrWorkspaceServiceDelivery, guestErr)
	}
	return nil
}

func sameWorkspaceServiceDesiredState(left, right domain.WorkspaceServiceCommand) bool {
	return left.SchemaVersion == right.SchemaVersion && left.TenantID == right.TenantID && left.WorkspaceID == right.WorkspaceID &&
		left.ResourcePlaneID == right.ResourcePlaneID && left.ServiceID == right.ServiceID && left.Revision == right.Revision &&
		workspaceServiceCredentialGeneration(left) == workspaceServiceCredentialGeneration(right) &&
		left.DesiredState == right.DesiredState && left.CatalogVersion == right.CatalogVersion && bytes.Equal(left.Config, right.Config)
}

func workspaceServiceCredentialGeneration(command domain.WorkspaceServiceCommand) int64 {
	if command.CredentialGeneration == 0 {
		return 1
	}
	return command.CredentialGeneration
}

func validWorkspaceServiceCommand(command domain.WorkspaceServiceCommand) bool {
	if command.SchemaVersion != 1 || command.TenantID == "" || command.WorkspaceID == "" || command.ResourcePlaneID == "" ||
		command.ServiceID == "" || command.Revision < 1 || command.CredentialGeneration < 0 || command.CatalogVersion == "" || len(command.Config) == 0 ||
		(command.DesiredState != "running" && command.DesiredState != "stopped" && command.DesiredState != "deleted") {
		return false
	}
	template, err := servicecatalog.Decode(bytes.NewReader(command.Config))
	if err != nil || command.CatalogVersion != template.Name+"/"+template.Version {
		return false
	}
	builtin, err := servicecatalog.Resolve(template.Name)
	if err != nil {
		return false
	}
	canonical, err := servicecatalog.CanonicalJSON(builtin)
	return err == nil && bytes.Equal(canonical, command.Config)
}

func validWorkspaceServiceGeneratedSecrets(template servicecatalog.Template, values map[string]string) bool {
	if len(template.GeneratedSecrets) != len(values) {
		return false
	}
	for _, name := range template.GeneratedSecrets {
		value, ok := values[name]
		if !ok || !validGeneratedServiceSecret(value) {
			return false
		}
	}
	return true
}

func validWorkspaceServiceObservation(command domain.WorkspaceServiceCommand, observation workspaceServiceObservation) bool {
	if observation.ServiceID != command.ServiceID || observation.Revision != command.Revision || observation.DesiredState != command.DesiredState ||
		observation.ObservedRevision < 0 || observation.ObservedRevision > observation.Revision {
		return false
	}
	switch observation.ObservedState {
	case "healthy":
		return observation.DesiredState == "running" && observation.Healthy && observation.ObservedRevision == observation.Revision && observation.ErrorCode == ""
	case "unhealthy":
		return observation.DesiredState == "running" && !observation.Healthy && observation.ObservedRevision == observation.Revision && observation.ErrorCode == ""
	case "stopped":
		return observation.DesiredState == "stopped" && !observation.Healthy && observation.ObservedRevision == observation.Revision && observation.ErrorCode == ""
	case "deleted":
		return observation.DesiredState == "deleted" && !observation.Healthy && observation.ObservedRevision == observation.Revision && observation.ErrorCode == ""
	case "failed":
		return !observation.Healthy && observation.ObservedRevision < observation.Revision && observation.ErrorCode != ""
	default:
		return false
	}
}

type workspaceServiceHTTPGuest struct{ client *http.Client }

func NewWorkspaceServiceHTTPGuest(client *http.Client) WorkspaceServiceGuest {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return workspaceServiceHTTPGuest{client: noRedirectClient(client)}
}

func (g workspaceServiceHTTPGuest) Reconcile(ctx context.Context, endpoint, authToken, capability string,
	command domain.WorkspaceServiceCommand, secrets map[string]string) (workspaceServiceObservation, error) {
	var observation workspaceServiceObservation
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		len(authToken) == 0 || len(authToken) > 8000 || strings.ContainsAny(authToken, " \t\r\n") || capability == "" || strings.ContainsAny(capability, " \t\r\n") {
		return observation, ErrWorkspaceServiceDelivery
	}
	body, err := json.Marshal(struct {
		Command domain.WorkspaceServiceCommand `json:"command"`
		Secrets map[string]string              `json:"secrets,omitempty"`
	}{Command: command, Secrets: secrets})
	if err != nil || len(body) > 64<<10 {
		return observation, ErrWorkspaceServiceDelivery
	}
	defer clear(body)
	parsed.Path = "/v1/workspaces/" + url.PathEscape(string(command.WorkspaceID)) + "/services/reconcile"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return observation, ErrWorkspaceServiceDelivery
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-aws-proxy-auth", authToken)
	request.Header.Set("Authorization", "Bearer "+capability)
	response, err := g.client.Do(request)
	if err != nil {
		return observation, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxWorkspaceServiceReplyBytes+1))
	if err != nil || len(responseBody) == 0 || len(responseBody) > maxWorkspaceServiceReplyBytes {
		return observation, ErrWorkspaceServiceDelivery
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&observation) != nil || decoder.Decode(new(any)) != io.EOF {
		return workspaceServiceObservation{}, ErrWorkspaceServiceDelivery
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return observation, fmt.Errorf("Guest Service handler returned HTTP %d", response.StatusCode)
	}
	return observation, nil
}

// SignedWorkspaceServiceReportSink sends reports to the Control Plane using
// the same canonical HMAC request format as its internal Resource Plane API.
type SignedWorkspaceServiceReportSink struct {
	endpoint string
	key      []byte
	client   *http.Client
	newID    func() ([]byte, error)
	now      func() time.Time
}

func NewSignedWorkspaceServiceReportSink(endpoint string, key []byte, client *http.Client, now func() time.Time) (*SignedWorkspaceServiceReportSink, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || len(key) < 32 {
		return nil, errors.New("Workspace Service report endpoint and HMAC key are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	client = noRedirectClient(client)
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &SignedWorkspaceServiceReportSink{endpoint: strings.TrimRight(endpoint, "/"), key: append([]byte(nil), key...), client: client,
		newID: func() ([]byte, error) { raw := make([]byte, 24); _, err := rand.Read(raw); return raw, err }, now: now}, nil
}

func (s *SignedWorkspaceServiceReportSink) Report(ctx context.Context, report WorkspaceServiceRuntimeReport) error {
	if s == nil || ctx == nil || report.TenantID == "" || report.WorkspaceID == "" || report.ResourcePlaneID == "" ||
		report.MicroVMID == "" || report.AllocationGeneration < 1 || report.ServiceID == "" || report.Revision < 1 || report.ObservedAt.IsZero() {
		return ErrWorkspaceServiceDelivery
	}
	body, err := json.Marshal(report)
	if err != nil || len(body) > 8<<10 {
		return ErrWorkspaceServiceDelivery
	}
	rawID, err := s.newID()
	if err != nil || len(rawID) != 24 {
		return ErrWorkspaceServiceDelivery
	}
	reportID := hex.EncodeToString(rawID)
	timestamp := fmt.Sprint(s.now().UTC().Unix())
	path := "/v1/internal/resource-planes/" + url.PathEscape(string(report.ResourcePlaneID)) + "/workspace-services/" + url.PathEscape(string(report.ServiceID)) + "/reports"
	bodyHash := sha256.Sum256(body)
	message := strings.Join([]string{http.MethodPost, path, timestamp, reportID, hex.EncodeToString(bodyHash[:])}, "\n")
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(message))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return ErrWorkspaceServiceDelivery
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hako-Timestamp", timestamp)
	request.Header.Set("X-Hako-Report-ID", reportID)
	request.Header.Set("X-Hako-Signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
		return fmt.Errorf("Control Plane Service report returned HTTP %d", response.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (8<<10)+1))
	if err != nil || len(responseBody) > 8<<10 {
		return ErrWorkspaceServiceDelivery
	}
	var result struct {
		Accepted bool  `json:"accepted"`
		Revision int64 `json:"revision"`
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || !result.Accepted || result.Revision != report.Revision {
		return ErrWorkspaceServiceDelivery
	}
	return nil
}

type WorkspaceServiceLambdaHandler struct {
	consumer WorkspaceServiceCommandRuntime
}

func NewWorkspaceServiceLambdaHandler(consumer WorkspaceServiceCommandRuntime) (*WorkspaceServiceLambdaHandler, error) {
	if consumer == nil {
		return nil, errors.New("Workspace Service command consumer is required")
	}
	return &WorkspaceServiceLambdaHandler{consumer: consumer}, nil
}

func (h *WorkspaceServiceLambdaHandler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	if h == nil || h.consumer == nil {
		return events.SQSEventResponse{}, errors.New("Workspace Service Lambda handler is not configured")
	}
	response := events.SQSEventResponse{BatchItemFailures: make([]events.SQSBatchItemFailure, 0)}
	for _, record := range event.Records {
		if err := h.consumer.Handle(ctx, []byte(record.Body)); err != nil {
			response.BatchItemFailures = append(response.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return response, nil
}

func noRedirectClient(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
