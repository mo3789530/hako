// hako-workload-controller-lambda is the Resource Plane-only Lambda entrypoint
// for Workload command and scheduled recovery events. It never connects to the
// Control Plane database; authorization and immutable Run metadata come from
// the authenticated metadata endpoint and Tenant checkout broker.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/artifactretention"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadcontroller"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

const maxProfileConfigBytes = 32 << 10

type profileDocument struct {
	Kind              domain.WorkloadKind       `json:"kind,omitempty"`
	RuntimeClass      string                    `json:"runtime_class"`
	TrustLevel        domain.WorkloadTrustLevel `json:"trust_level"`
	ImageIdentifier   string                    `json:"image_identifier"`
	ImageVersion      string                    `json:"image_version"`
	ImageDigest       string                    `json:"image_digest"`
	JobMemoryLimitMB  int                       `json:"job_memory_limit_mb,omitempty"`
	JobCPULimitMillis int                       `json:"job_cpu_limit_millis,omitempty"`
	IngressConnector  string                    `json:"ingress_connector"`
	EgressConnector   string                    `json:"egress_connector"`
}

type deploymentConfig struct {
	Mode                      string
	ResourcePlaneID           domain.ResourcePlaneID
	ControlPlaneEndpoint      string
	ControlPlaneHMACKeyARN    string
	ArtifactCleanupHMACKeyARN string
	BrokerURL                 string
	BrokerCertSecretARN       string
	BrokerKeySecretARN        string
	BrokerCASecretARN         string
	JournalBucket             string
	JournalPrefix             string
	RecoveryLeaseKey          string
	SourceBucket              string
	SourcePrefix              string
	SourceKMSKeyID            string
	ArtifactBucket            string
	ArtifactPrefix            string
	ArtifactKMSKeyID          string
	ResultQueueURL            string
	TicketKeyID               string
	TicketPrivateKeyARN       string
	Profiles                  map[workloadcontroller.RuntimeProfileKey]workloadcontroller.RuntimeProfile
	Runtime                   workloadcontroller.LambdaConfig
	Recovery                  workloadcontroller.RecoveryLambdaConfig
	ArtifactCleanupBatch      int
}

func main() {
	if err := run(); err != nil {
		log.Fatal("Hako Workload controller Lambda stopped (failure_class=process_exit)")
	}
}

func run() error {
	config, err := configuredDeployment(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return errors.New("load Resource Plane Workload AWS configuration")
	}
	controllerConfig, publisher, lease, cleanupWorker, err := compose(ctx, awsCfg, config)
	if err != nil {
		return err
	}
	if config.Mode == "command" {
		handler, err := workloadcontroller.NewLambdaWorkloadHandler(controllerConfig)
		if err != nil {
			return errors.New("configure Workload command handler")
		}
		lambda.Start(handler.Handle)
		return nil
	}
	recovery, err := workloadcontroller.NewLambdaWorkloadRecoveryHandler(workloadcontroller.LambdaWorkloadRecoveryHandlerConfig{
		Workload: controllerConfig, Leases: lease, Publisher: publisher, Recovery: config.Recovery,
	})
	if err != nil {
		return errors.New("configure Workload recovery handler")
	}
	if config.Mode == "combined" {
		command, err := workloadcontroller.NewLambdaWorkloadHandler(controllerConfig)
		if err != nil {
			return errors.New("configure Workload command handler")
		}
		recoveryHandler := func(ctx context.Context, event events.CloudWatchEvent) (any, error) {
			return recovery.Handle(ctx, event)
		}
		cleanupHandler := func(ctx context.Context) (any, error) {
			return cleanupWorker.RunOnce(ctx, time.Now().UTC())
		}
		lambda.Start(newEventMux(command.Handle, recoveryHandler, cleanupHandler))
		return nil
	}
	lambda.Start(recovery.Handle)
	return nil
}

func compose(ctx context.Context, awsCfg aws.Config, config deploymentConfig) (workloadcontroller.LambdaWorkloadHandlerConfig,
	*workloadcontroller.SQSResultPublisher, *workloadjournal.S3RecoveryLease, *artifactretention.Worker, error) {
	if ctx == nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("Workload composition context is required")
	}
	secretReader := secretsmanager.NewFromConfig(awsCfg)
	metadataKey, err := readSecret(ctx, secretReader, config.ControlPlaneHMACKeyARN, 4096)
	if err != nil || len(metadataKey) < 32 {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("load Resource Plane metadata HMAC key")
	}
	defer wipeSecretBytes(metadataKey)
	cleanupKey, err := readSecret(ctx, secretReader, config.ArtifactCleanupHMACKeyARN, 4096)
	if err != nil || len(cleanupKey) < 32 {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("load Resource Plane artifact cleanup HMAC key")
	}
	defer wipeSecretBytes(cleanupKey)
	ticketKeyBytes, err := readSecret(ctx, secretReader, config.TicketPrivateKeyARN, 16<<10)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("load Agent ticket signing key secret")
	}
	defer wipeSecretBytes(ticketKeyBytes)
	ticketKey, err := workloadagent.LoadTicketSigningKey(bytes.NewReader(ticketKeyBytes))
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("parse Agent ticket signing key secret")
	}
	certPEM, err := readSecret(ctx, secretReader, config.BrokerCertSecretARN, 64<<10)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("load checkout broker client certificate secret")
	}
	keyPEM, err := readSecret(ctx, secretReader, config.BrokerKeySecretARN, 64<<10)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("load checkout broker client key secret")
	}
	defer wipeSecretBytes(keyPEM)
	caPEM, err := readSecret(ctx, secretReader, config.BrokerCASecretARN, 64<<10)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("load checkout broker CA secret")
	}
	certificate, roots, err := checkoutTLSFromPEM(certPEM, keyPEM, caPEM)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure checkout broker mTLS")
	}
	brokerClient, err := githubbroker.NewResourcePlaneCheckoutMTLSClient(string(config.ResourcePlaneID), certificate, roots)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure checkout broker client")
	}
	checkoutCredentials, err := githubbroker.NewHTTPCheckoutProvider(config.BrokerURL, brokerClient)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure checkout broker endpoint")
	}
	agentTaskClient, err := githubbroker.NewHTTPAgentTaskClient(config.BrokerURL, string(config.ResourcePlaneID), certificate, roots)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure Agent task broker client")
	}
	awsS3 := s3.NewFromConfig(awsCfg)
	journal, err := workloadjournal.NewS3Store(awsS3, config.JournalBucket, config.JournalPrefix)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure Workload journal store")
	}
	sourceStager, err := workloadsource.NewS3SourceArchiveStager(awsS3, config.SourceBucket, config.SourcePrefix, config.SourceKMSKeyID, 0)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure source archive stager")
	}
	sources, err := workloadcontroller.NewTenantBrokeredRepositorySourceMaterializer(checkoutCredentials, sourceStager, nil)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure Tenant-pinned source materializer")
	}
	metadata, err := workloadcontroller.NewControlPlaneMetadataClient(workloadcontroller.ControlPlaneMetadataConfig{
		Endpoint: config.ControlPlaneEndpoint, ResourcePlaneID: config.ResourcePlaneID, HMACKey: metadataKey,
	})
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure Control Plane metadata client")
	}
	cleanupClient, err := artifactretention.NewControlPlaneClient(artifactretention.ControlPlaneClientConfig{
		Endpoint: config.ControlPlaneEndpoint, ResourcePlaneID: config.ResourcePlaneID, HMACKey: cleanupKey,
	})
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure Control Plane artifact cleanup client")
	}
	cleanupWorker, err := artifactretention.New(cleanupClient, awsS3, artifactretention.Config{
		Bucket: config.ArtifactBucket, KeyPrefix: strings.TrimSuffix(config.ArtifactPrefix, "/") + "/", BatchSize: config.ArtifactCleanupBatch,
	})
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure Workload artifact cleanup")
	}
	resolver, err := workloadcontroller.NewMetadataSpecificationResolver(workloadcontroller.MetadataSpecificationResolverConfig{
		ResourcePlaneID: config.ResourcePlaneID, Metadata: metadata, Sources: sources, Profiles: config.Profiles,
	})
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure trusted Workload specification resolver")
	}
	archiveReader, err := workloadsource.NewS3SourceArchiveReader(awsS3, config.SourceBucket, config.SourcePrefix, 0)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure source archive reader")
	}
	logWriter, err := workloadagent.NewS3AttemptLogWriter(awsS3, config.ArtifactBucket, config.ArtifactPrefix, config.ArtifactKMSKeyID)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure attempt log writer")
	}
	resultPublisher, err := workloadcontroller.NewSQSResultPublisher(sqs.NewFromConfig(awsCfg), config.ResultQueueURL)
	if err != nil {
		return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure Workload result queue publisher")
	}
	ticketIssuer := &workloadagent.Ed25519TicketIssuer{KeyID: config.TicketKeyID, PrivateKey: ticketKey, TTL: workloadagent.AgentTicketMaxTTL}
	controllerConfig := workloadcontroller.LambdaWorkloadHandlerConfig{
		ResourcePlaneID: string(config.ResourcePlaneID), Journal: journal, LambdaAPI: lambdamicrovms.NewFromConfig(awsCfg), Resolver: resolver,
		SourceArchives: archiveReader, AttemptLogs: logWriter, AgentTasks: brokerAgentTaskProvider{client: agentTaskClient},
		Runtime: workloadcontroller.LambdaConfig{ImageIdentifier: config.Runtime.ImageIdentifier, ImageVersion: config.Runtime.ImageVersion,
			ImageDigest: config.Runtime.ImageDigest, IngressConnector: config.Runtime.IngressConnector,
			VPCEgressConnector: config.Runtime.VPCEgressConnector, AgentTicketIssuer: ticketIssuer},
	}
	var lease *workloadjournal.S3RecoveryLease
	if config.Mode == "recovery" || config.Mode == "combined" {
		lease, err = workloadjournal.NewS3RecoveryLease(awsS3, config.JournalBucket, config.RecoveryLeaseKey)
		if err != nil {
			return workloadcontroller.LambdaWorkloadHandlerConfig{}, nil, nil, nil, errors.New("configure Workload recovery lease")
		}
	}
	return controllerConfig, resultPublisher, lease, cleanupWorker, nil
}

type brokerAgentTaskProvider struct {
	client *githubbroker.HTTPAgentTaskClient
}

func (p brokerAgentTaskProvider) ResolveAgentTask(ctx context.Context, id workloadjournal.Command) (workloadagent.AgentTask, error) {
	if p.client == nil {
		return workloadagent.AgentTask{}, errors.New("Agent task broker is unavailable")
	}
	record, err := p.client.Resolve(ctx, githubbroker.AgentTaskIdentity{TenantID: domain.TenantID(id.TenantID),
		WorkloadRunID: domain.WorkloadRunID(id.WorkloadRunID), AttemptID: id.AttemptID, Generation: id.Generation,
		ResourcePlaneID: domain.ResourcePlaneID(id.ResourcePlaneID)})
	if err != nil {
		return workloadagent.AgentTask{}, errors.New("Agent task broker resolution failed")
	}
	return workloadagent.AgentTask{TenantID: string(record.TenantID), WorkloadRunID: string(record.WorkloadRunID),
		IntegrationID: record.IntegrationID, DeliveryID: record.DeliveryID, InstallationID: record.InstallationID,
		RepositoryID: record.RepositoryID, IssueNumber: record.IssueNumber, CommentID: record.CommentID,
		DefaultBranch: record.DefaultBranch, CommitSHA: record.CommitSHA, Title: record.Title, Body: record.Body,
		ContextSHA256: record.ContextSHA256}, nil
}

func configuredDeployment(getenv func(string) string) (deploymentConfig, error) {
	if getenv == nil {
		return deploymentConfig{}, errors.New("environment reader is required")
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	cfg := deploymentConfig{
		Mode: get("HAKO_WORKLOAD_LAMBDA_MODE"), ResourcePlaneID: domain.ResourcePlaneID(get("HAKO_RESOURCE_PLANE_ID")),
		ControlPlaneEndpoint: get("HAKO_WORKLOAD_CONTROL_PLANE_ENDPOINT"), ControlPlaneHMACKeyARN: get("HAKO_WORKLOAD_CONTROL_PLANE_HMAC_SECRET_ARN"),
		ArtifactCleanupHMACKeyARN: get("HAKO_WORKLOAD_ARTIFACT_CLEANUP_HMAC_SECRET_ARN"),
		BrokerURL:                 get("HAKO_WORKLOAD_CHECKOUT_BROKER_URL"), BrokerCertSecretARN: get("HAKO_WORKLOAD_BROKER_CLIENT_CERT_SECRET_ARN"),
		BrokerKeySecretARN: get("HAKO_WORKLOAD_BROKER_CLIENT_KEY_SECRET_ARN"), BrokerCASecretARN: get("HAKO_WORKLOAD_BROKER_CA_SECRET_ARN"),
		JournalBucket: get("HAKO_WORKLOAD_JOURNAL_BUCKET"), JournalPrefix: get("HAKO_WORKLOAD_JOURNAL_PREFIX"),
		RecoveryLeaseKey: get("HAKO_WORKLOAD_RECOVERY_LEASE_KEY"), SourceBucket: get("HAKO_WORKLOAD_SOURCE_BUCKET"),
		SourcePrefix: get("HAKO_WORKLOAD_SOURCE_PREFIX"), SourceKMSKeyID: get("HAKO_WORKLOAD_SOURCE_KMS_KEY_ID"),
		ArtifactBucket: get("HAKO_WORKLOAD_ARTIFACT_BUCKET"), ArtifactPrefix: get("HAKO_WORKLOAD_ARTIFACT_PREFIX"),
		ArtifactKMSKeyID: get("HAKO_WORKLOAD_ARTIFACT_KMS_KEY_ID"), ResultQueueURL: get("HAKO_WORKLOAD_RESULT_QUEUE_URL"),
		TicketKeyID: get("HAKO_WORKLOAD_TICKET_KEY_ID"), TicketPrivateKeyARN: get("HAKO_WORKLOAD_TICKET_PRIVATE_KEY_SECRET_ARN"),
		ArtifactCleanupBatch: 100,
	}
	if cfg.Mode != "command" && cfg.Mode != "recovery" && cfg.Mode != "combined" {
		return deploymentConfig{}, errors.New("HAKO_WORKLOAD_LAMBDA_MODE must be command, recovery, or combined")
	}
	if cfg.ResourcePlaneID == "" || cfg.ControlPlaneEndpoint == "" || !validSecretARN(cfg.ControlPlaneHMACKeyARN) ||
		!validSecretARN(cfg.ArtifactCleanupHMACKeyARN) || cfg.BrokerURL == "" ||
		!validSecretARN(cfg.BrokerCertSecretARN) || !validSecretARN(cfg.BrokerKeySecretARN) || !validSecretARN(cfg.BrokerCASecretARN) || cfg.JournalBucket == "" ||
		cfg.SourceBucket == "" || cfg.ArtifactBucket == "" || cfg.ResultQueueURL == "" || cfg.TicketKeyID == "" || !validSecretARN(cfg.TicketPrivateKeyARN) {
		return deploymentConfig{}, errors.New("Workload Lambda Resource Plane, metadata, broker, S3, result queue, and ticket settings are required")
	}
	if rawBatch := get("HAKO_WORKLOAD_ARTIFACT_CLEANUP_BATCH_SIZE"); rawBatch != "" {
		batch, parseErr := positiveInt(rawBatch, 100, 1, artifactretention.MaxBatchSize)
		if parseErr != nil {
			return deploymentConfig{}, errors.New("HAKO_WORKLOAD_ARTIFACT_CLEANUP_BATCH_SIZE must be 1-1000")
		}
		cfg.ArtifactCleanupBatch = batch
	}
	secretARNs := []string{cfg.ControlPlaneHMACKeyARN, cfg.ArtifactCleanupHMACKeyARN, cfg.BrokerCertSecretARN, cfg.BrokerKeySecretARN, cfg.BrokerCASecretARN, cfg.TicketPrivateKeyARN}
	seenSecrets := make(map[string]struct{}, len(secretARNs))
	for _, arn := range secretARNs {
		if _, exists := seenSecrets[arn]; exists {
			return deploymentConfig{}, errors.New("Workload Lambda Secret references must be distinct")
		}
		seenSecrets[arn] = struct{}{}
	}
	profiles, err := parseProfiles(get("HAKO_WORKLOAD_RUNTIME_PROFILES_JSON"))
	if err != nil {
		return deploymentConfig{}, err
	}
	var base workloadcontroller.RuntimeProfile
	first := true
	for _, profile := range profiles {
		if first {
			base, first = profile, false
			continue
		}
		if profile.ImageIdentifier != base.ImageIdentifier || profile.ImageVersion != base.ImageVersion || profile.ImageDigest != base.ImageDigest ||
			profile.IngressNetworkConnectors[0] != base.IngressNetworkConnectors[0] || profile.EgressNetworkConnectors[0] != base.EgressNetworkConnectors[0] {
			return deploymentConfig{}, errors.New("one Workload Controller Lambda can only use one immutable image and network connector pair")
		}
	}
	cfg.Profiles = profiles
	cfg.Runtime = workloadcontroller.LambdaConfig{ImageIdentifier: base.ImageIdentifier, ImageVersion: base.ImageVersion, ImageDigest: base.ImageDigest,
		IngressConnector: base.IngressNetworkConnectors[0], VPCEgressConnector: base.EgressNetworkConnectors[0]}
	if cfg.Mode == "recovery" || cfg.Mode == "combined" {
		if cfg.RecoveryLeaseKey, err = requiredPathPrefix(cfg.RecoveryLeaseKey); err != nil {
			return deploymentConfig{}, errors.New("HAKO_WORKLOAD_RECOVERY_LEASE_KEY must be a non-empty dedicated S3 object key")
		}
		pageSize, parseErr := positiveInt(get("HAKO_WORKLOAD_RECOVERY_PAGE_SIZE"), 100, 1, 1000)
		if parseErr != nil {
			return deploymentConfig{}, errors.New("HAKO_WORKLOAD_RECOVERY_PAGE_SIZE must be 1-1000")
		}
		maxPages, parseErr := positiveInt(get("HAKO_WORKLOAD_RECOVERY_MAX_PAGES"), 10, 1, 1000)
		if parseErr != nil {
			return deploymentConfig{}, errors.New("HAKO_WORKLOAD_RECOVERY_MAX_PAGES must be 1-1000")
		}
		leaseTTL, parseErr := time.ParseDuration(defaultString(get("HAKO_WORKLOAD_RECOVERY_LEASE_TTL"), "5m"))
		if parseErr != nil || leaseTTL < time.Second || leaseTTL > 30*time.Minute {
			return deploymentConfig{}, errors.New("HAKO_WORKLOAD_RECOVERY_LEASE_TTL must be 1s-30m")
		}
		cfg.Recovery = workloadcontroller.RecoveryLambdaConfig{PageSize: int32(pageSize), MaxPages: maxPages, LeaseTTL: leaseTTL}
	}
	return cfg, nil
}

func parseProfiles(raw string) (map[workloadcontroller.RuntimeProfileKey]workloadcontroller.RuntimeProfile, error) {
	if raw == "" || len(raw) > maxProfileConfigBytes {
		return nil, errors.New("HAKO_WORKLOAD_RUNTIME_PROFILES_JSON must contain bounded Resource Plane-owned runtime profiles")
	}
	var docs []profileDocument
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&docs) != nil || decoder.Decode(new(any)) != io.EOF || len(docs) == 0 || len(docs) > 32 {
		return nil, errors.New("HAKO_WORKLOAD_RUNTIME_PROFILES_JSON is invalid")
	}
	profiles := make(map[workloadcontroller.RuntimeProfileKey]workloadcontroller.RuntimeProfile, len(docs))
	for _, doc := range docs {
		kind := doc.Kind
		if kind == "" {
			kind = domain.WorkloadJob
		}
		key := workloadcontroller.RuntimeProfileKey{Kind: kind, RuntimeClass: strings.TrimSpace(doc.RuntimeClass), TrustLevel: doc.TrustLevel}
		if key.Kind != domain.WorkloadJob && key.Kind != domain.WorkloadAgent && key.Kind != domain.WorkloadPreview {
			return nil, errors.New("Workload runtime profile kind is invalid")
		}
		if key.RuntimeClass == "" || (key.TrustLevel != domain.WorkloadUntrusted && key.TrustLevel != domain.WorkloadTrusted && key.TrustLevel != domain.WorkloadPrivileged) {
			return nil, errors.New("Workload runtime profile identity is invalid")
		}
		if key.Kind == domain.WorkloadJob && (doc.JobMemoryLimitMB < 256 || doc.JobMemoryLimitMB > 16384 ||
			doc.JobCPULimitMillis < 250 || doc.JobCPULimitMillis > 8000) {
			return nil, errors.New("Workload Job runtime profile resource limits are invalid")
		}
		if key.Kind != domain.WorkloadJob && (doc.JobMemoryLimitMB != 0 || doc.JobCPULimitMillis != 0) {
			return nil, errors.New("Job resource limits are not valid for non-Job runtime profiles")
		}
		if _, exists := profiles[key]; exists {
			return nil, errors.New("Workload runtime profile identity is duplicated")
		}
		profile := workloadcontroller.RuntimeProfile{ImageIdentifier: strings.TrimSpace(doc.ImageIdentifier), ImageVersion: strings.TrimSpace(doc.ImageVersion),
			ImageDigest: strings.TrimSpace(doc.ImageDigest), IngressNetworkConnectors: []string{strings.TrimSpace(doc.IngressConnector)},
			EgressNetworkConnectors: []string{strings.TrimSpace(doc.EgressConnector)}, JobMemoryLimitMB: doc.JobMemoryLimitMB,
			JobCPULimitMillis: doc.JobCPULimitMillis}
		profiles[key] = profile
	}
	return profiles, nil
}

func checkoutTLSFromPEM(certPEM, keyPEM, caPEM []byte) (tls.Certificate, *x509.CertPool, error) {
	if len(certPEM) == 0 || len(certPEM) > 64<<10 {
		return tls.Certificate{}, nil, errors.New("invalid broker client certificate material")
	}
	if len(keyPEM) == 0 || len(keyPEM) > 64<<10 {
		return tls.Certificate{}, nil, errors.New("invalid broker client key material")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid broker client key pair")
	}
	certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid broker client leaf certificate")
	}
	if len(caPEM) == 0 || len(caPEM) > 64<<10 {
		return tls.Certificate{}, nil, errors.New("invalid broker CA material")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("invalid broker CA bundle")
	}
	return certificate, roots, nil
}

func wipeSecretBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func requiredPathPrefix(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "..") || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("invalid S3 object key")
	}
	return value, nil
}

func positiveInt(raw string, fallback, min, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, errors.New("integer is outside the configured range")
	}
	return value, nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
