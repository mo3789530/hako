// hako-agentcore-worker-lambda is the Resource Plane SQS consumer for
// AgentCore Workload attempts. Repository code runs in AgentCore Runtime;
// this Lambda only resolves pinned inputs, invokes the Runtime, checkpoints
// results, confirms session cleanup, and publishes metadata-only results.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadagentcore"
	"github.com/mo3789530/hako/internal/workloadcontroller"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

var (
	secretARNPattern     = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]{1,512}$`)
	resourcePlanePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	workerBucketPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	workerPrefixPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
)

type deploymentConfig struct {
	ResourcePlaneID string
	BrokerURL       string
	BrokerCertARN   string
	BrokerKeyARN    string
	BrokerCAARN     string
	JournalBucket   string
	JournalPrefix   string
	SourceBucket    string
	SourcePrefix    string
	ArtifactBucket  string
	ArtifactPrefix  string
	ArtifactKMSKey  string
	ResultQueueURL  string
}

func main() {
	if err := run(); err != nil {
		log.Fatal("Hako AgentCore worker Lambda stopped (failure_class=process_exit)")
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
		return errors.New("load Resource Plane AgentCore worker AWS configuration")
	}
	secrets := secretsmanager.NewFromConfig(awsCfg)
	certificatePEM, err := readSecret(ctx, secrets, config.BrokerCertARN, 64<<10)
	if err != nil {
		return errors.New("load Agent task broker client certificate")
	}
	keyPEM, err := readSecret(ctx, secrets, config.BrokerKeyARN, 64<<10)
	if err != nil {
		wipeSecretBytes(certificatePEM)
		return errors.New("load Agent task broker client key")
	}
	defer wipeSecretBytes(certificatePEM)
	defer wipeSecretBytes(keyPEM)
	caPEM, err := readSecret(ctx, secrets, config.BrokerCAARN, 64<<10)
	if err != nil {
		return errors.New("load Agent task broker CA")
	}
	defer wipeSecretBytes(caPEM)
	certificate, roots, err := brokerTLSFromPEM(certificatePEM, keyPEM, caPEM)
	if err != nil {
		return errors.New("configure Agent task broker mTLS")
	}
	taskClient, err := githubbroker.NewHTTPAgentTaskClient(config.BrokerURL, config.ResourcePlaneID, certificate, roots)
	if err != nil {
		return errors.New("configure Agent task broker client")
	}
	s3Client := s3.NewFromConfig(awsCfg)
	journal, err := workloadjournal.NewS3Store(s3Client, config.JournalBucket, config.JournalPrefix)
	if err != nil {
		return errors.New("configure AgentCore attempt journal")
	}
	sources, err := workloadsource.NewS3SourceArchiveReader(s3Client, config.SourceBucket, config.SourcePrefix, 40<<20)
	if err != nil {
		return errors.New("configure attempt source archive reader")
	}
	inputs, err := workloadcontroller.NewBrokeredAgentCoreInvocationResolver(journal,
		brokerAgentTaskProvider{client: taskClient}, sources)
	if err != nil {
		return errors.New("configure attempt-pinned AgentCore input resolver")
	}
	artifacts, err := workloadagent.NewS3AttemptChangeBundleWriter(s3Client, config.ArtifactBucket,
		config.ArtifactPrefix, config.ArtifactKMSKey)
	if err != nil {
		return errors.New("configure AgentCore proposal artifact writer")
	}
	publisher, err := workloadcontroller.NewSQSResultPublisher(sqs.NewFromConfig(awsCfg), config.ResultQueueURL)
	if err != nil {
		return errors.New("configure Control Plane Workload result publisher")
	}
	provider, err := workloadagentcore.NewAWSRuntimeProvider(bedrockagentcore.NewFromConfig(awsCfg))
	if err != nil {
		return errors.New("configure AgentCore Runtime provider")
	}
	worker, err := workloadcontroller.NewAgentCoreWorkloadWorker(workloadcontroller.AgentCoreWorkloadWorkerConfig{
		Journal: journal, Inputs: inputs, Invoker: provider, Stopper: provider, Artifacts: artifacts,
		Publisher: publisher, LeaseTTL: 20 * time.Minute,
	})
	if err != nil {
		return errors.New("configure AgentCore Workload worker")
	}
	handler, err := workloadcontroller.NewAgentCoreLambdaHandler(worker)
	if err != nil {
		return errors.New("configure AgentCore SQS handler")
	}
	lambda.Start(handler.Handle)
	return nil
}

type brokerAgentTaskProvider struct {
	client *githubbroker.HTTPAgentTaskClient
}

func (p brokerAgentTaskProvider) ResolveAgentTask(ctx context.Context, command workloadjournal.Command) (workloadagent.AgentTask, error) {
	if p.client == nil {
		return workloadagent.AgentTask{}, errors.New("Agent task broker is unavailable")
	}
	record, err := p.client.Resolve(ctx, githubbroker.AgentTaskIdentity{TenantID: domain.TenantID(command.TenantID),
		WorkloadRunID: domain.WorkloadRunID(command.WorkloadRunID), AttemptID: command.AttemptID,
		Generation: command.Generation, ResourcePlaneID: domain.ResourcePlaneID(command.ResourcePlaneID)})
	if err != nil {
		return workloadagent.AgentTask{}, err
	}
	return workloadagent.AgentTask{TenantID: string(record.TenantID), WorkloadRunID: string(record.WorkloadRunID),
		IntegrationID: record.IntegrationID, DeliveryID: record.DeliveryID,
		InstallationID: record.InstallationID, RepositoryID: record.RepositoryID, IssueNumber: record.IssueNumber, CommentID: record.CommentID,
		CommitSHA: record.CommitSHA, DefaultBranch: record.DefaultBranch, ContextSHA256: record.ContextSHA256,
		Title: record.Title, Body: record.Body}, nil
}

func configuredDeployment(getenv func(string) string) (deploymentConfig, error) {
	if getenv == nil {
		return deploymentConfig{}, errors.New("AgentCore worker environment is unavailable")
	}
	config := deploymentConfig{
		ResourcePlaneID: strings.TrimSpace(getenv("HAKO_AGENTCORE_RESOURCE_PLANE_ID")),
		BrokerURL:       strings.TrimSpace(getenv("HAKO_AGENTCORE_BROKER_URL")),
		BrokerCertARN:   strings.TrimSpace(getenv("HAKO_AGENTCORE_BROKER_CERT_SECRET_ARN")),
		BrokerKeyARN:    strings.TrimSpace(getenv("HAKO_AGENTCORE_BROKER_KEY_SECRET_ARN")),
		BrokerCAARN:     strings.TrimSpace(getenv("HAKO_AGENTCORE_BROKER_CA_SECRET_ARN")),
		JournalBucket:   strings.TrimSpace(getenv("HAKO_AGENTCORE_JOURNAL_BUCKET")),
		JournalPrefix:   strings.TrimSpace(getenv("HAKO_AGENTCORE_JOURNAL_PREFIX")),
		SourceBucket:    strings.TrimSpace(getenv("HAKO_AGENTCORE_SOURCE_BUCKET")),
		SourcePrefix:    strings.TrimSpace(getenv("HAKO_AGENTCORE_SOURCE_PREFIX")),
		ArtifactBucket:  strings.TrimSpace(getenv("HAKO_AGENTCORE_ARTIFACT_BUCKET")),
		ArtifactPrefix:  strings.TrimSpace(getenv("HAKO_AGENTCORE_ARTIFACT_PREFIX")),
		ArtifactKMSKey:  strings.TrimSpace(getenv("HAKO_AGENTCORE_ARTIFACT_KMS_KEY_ID")),
		ResultQueueURL:  strings.TrimSpace(getenv("HAKO_AGENTCORE_RESULT_QUEUE_URL")),
	}
	parsed, err := url.Parse(config.BrokerURL)
	resultURL, resultErr := url.Parse(config.ResultQueueURL)
	if !resourcePlanePattern.MatchString(config.ResourcePlaneID) || err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!secretARNPattern.MatchString(config.BrokerCertARN) || !secretARNPattern.MatchString(config.BrokerKeyARN) ||
		!secretARNPattern.MatchString(config.BrokerCAARN) || config.BrokerCertARN == config.BrokerKeyARN ||
		config.BrokerCertARN == config.BrokerCAARN || config.BrokerKeyARN == config.BrokerCAARN ||
		!validWorkerBucket(config.JournalBucket) || !validWorkerBucket(config.SourceBucket) || !validWorkerBucket(config.ArtifactBucket) ||
		!validWorkerPrefix(config.JournalPrefix) || !validWorkerPrefix(config.SourcePrefix) || !validWorkerPrefix(config.ArtifactPrefix) ||
		resultErr != nil || resultURL.Scheme != "https" || resultURL.Host == "" || resultURL.User != nil ||
		resultURL.RawQuery != "" || resultURL.Fragment != "" || resultURL.Path == "" ||
		len(config.ArtifactKMSKey) > 2048 || strings.ContainsAny(config.ArtifactKMSKey, "\r\n\x00") {
		return deploymentConfig{}, errors.New("AgentCore worker deployment configuration is invalid")
	}
	return config, nil
}

func validWorkerBucket(value string) bool {
	return workerBucketPattern.MatchString(value) && !strings.Contains(value, "..") &&
		!strings.Contains(value, ".-") && !strings.Contains(value, "-.")
}

func validWorkerPrefix(value string) bool {
	return value == "" || (workerPrefixPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.Contains(value, "//"))
}

type secretValueAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

func readSecret(ctx context.Context, client secretValueAPI, arn string, maxBytes int) ([]byte, error) {
	if ctx == nil || client == nil || !secretARNPattern.MatchString(arn) || maxBytes < 1 {
		return nil, errors.New("invalid Secret reference")
	}
	output, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &arn})
	if err != nil || output == nil || (output.SecretString == nil) == (output.SecretBinary == nil) {
		return nil, errors.New("Secret value unavailable")
	}
	var value []byte
	if output.SecretString != nil {
		value = []byte(*output.SecretString)
	} else {
		value = append([]byte(nil), output.SecretBinary...)
	}
	if len(value) == 0 || len(value) > maxBytes {
		wipeSecretBytes(value)
		return nil, errors.New("Secret value is empty or oversized")
	}
	return value, nil
}

func brokerTLSFromPEM(certPEM, keyPEM, caPEM []byte) (tls.Certificate, *x509.CertPool, error) {
	if len(certPEM) == 0 || len(certPEM) > 64<<10 || len(keyPEM) == 0 || len(keyPEM) > 64<<10 ||
		len(caPEM) == 0 || len(caPEM) > 64<<10 {
		return tls.Certificate{}, nil, errors.New("invalid broker certificate material")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid broker client key pair")
	}
	certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid broker client leaf certificate")
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
