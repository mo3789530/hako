package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/mo3789530/hako/internal/actionsrunner"
	"github.com/mo3789530/hako/internal/githubbroker"
)

type cleanupDeploymentConfig struct {
	ResourcePlaneID     string
	BrokerURL           string
	BrokerClientCertARN string
	BrokerClientKeyARN  string
	BrokerCAARN         string
	LeaseDuration       time.Duration
	OperationTimeout    time.Duration
	PollInterval        time.Duration
	PollTimeout         time.Duration
	BatchSize           int
}

func runCleanup() error {
	config, err := configuredCleanupDeployment(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return errors.New("load Actions runner cleanup Resource Plane AWS configuration")
	}
	handler, err := buildCleanupHandler(ctx, config, secretsmanager.NewFromConfig(awsConfig), lambdamicrovms.NewFromConfig(awsConfig))
	if err != nil {
		return errors.New("compose Actions runner cleanup handler")
	}
	lambda.Start(handler.Handle)
	return nil
}

func configuredCleanupDeployment(getenv func(string) string) (cleanupDeploymentConfig, error) {
	if getenv == nil {
		return cleanupDeploymentConfig{}, errors.New("environment reader is required")
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	config := cleanupDeploymentConfig{
		ResourcePlaneID: get("HAKO_RESOURCE_PLANE_ID"), BrokerURL: get("HAKO_ACTIONS_RUNNER_BROKER_URL"),
		BrokerClientCertARN: get("HAKO_ACTIONS_RUNNER_BROKER_CLIENT_CERT_SECRET_ARN"),
		BrokerClientKeyARN:  get("HAKO_ACTIONS_RUNNER_BROKER_CLIENT_KEY_SECRET_ARN"),
		BrokerCAARN:         get("HAKO_ACTIONS_RUNNER_BROKER_CA_SECRET_ARN"),
	}
	if config.ResourcePlaneID == "" || !validSecretARN(config.BrokerClientCertARN) || !validSecretARN(config.BrokerClientKeyARN) ||
		!validSecretARN(config.BrokerCAARN) || config.BrokerURL == "" {
		return cleanupDeploymentConfig{}, errors.New("Resource Plane, HTTPS broker, and three broker Secret references are required")
	}
	client, err := githubbroker.NewHTTPActionsRunnerCleanupQueueClient(config.BrokerURL, config.ResourcePlaneID, &http.Client{Timeout: 15 * time.Second})
	if err != nil || client == nil {
		return cleanupDeploymentConfig{}, errors.New("Actions runner cleanup broker URL or Resource Plane identity is invalid")
	}
	if config.BrokerClientCertARN == config.BrokerClientKeyARN || config.BrokerClientCertARN == config.BrokerCAARN || config.BrokerClientKeyARN == config.BrokerCAARN {
		return cleanupDeploymentConfig{}, errors.New("Actions runner cleanup broker Secret references must be distinct")
	}
	config.LeaseDuration, err = boundedDuration(get("HAKO_ACTIONS_RUNNER_CLEANUP_LEASE"), 2*time.Minute, 5*time.Second, 10*time.Minute)
	if err != nil {
		return cleanupDeploymentConfig{}, errors.New("HAKO_ACTIONS_RUNNER_CLEANUP_LEASE must be 5s-10m")
	}
	config.OperationTimeout, err = boundedDuration(get("HAKO_ACTIONS_RUNNER_CLEANUP_OPERATION_TIMEOUT"), 90*time.Second, time.Second, 5*time.Minute)
	if err != nil || config.OperationTimeout+5*time.Second > config.LeaseDuration {
		return cleanupDeploymentConfig{}, errors.New("cleanup operation timeout must leave at least five seconds in the lease")
	}
	config.PollInterval, err = boundedDuration(get("HAKO_ACTIONS_RUNNER_CLEANUP_POLL_INTERVAL"), 250*time.Millisecond, 10*time.Millisecond, time.Minute)
	if err != nil {
		return cleanupDeploymentConfig{}, errors.New("cleanup poll interval must be 10ms-1m")
	}
	config.PollTimeout, err = boundedDuration(get("HAKO_ACTIONS_RUNNER_CLEANUP_POLL_TIMEOUT"), 30*time.Second, config.PollInterval, 10*time.Minute)
	if err != nil || config.PollTimeout > config.OperationTimeout {
		return cleanupDeploymentConfig{}, errors.New("cleanup poll timeout must not exceed the operation timeout")
	}
	config.BatchSize, err = boundedInt(get("HAKO_ACTIONS_RUNNER_CLEANUP_BATCH_SIZE"), 20, 1, 100)
	if err != nil {
		return cleanupDeploymentConfig{}, errors.New("cleanup batch size must be 1-100")
	}
	return config, nil
}

func buildCleanupHandler(ctx context.Context, config cleanupDeploymentConfig, secrets secretAPI, api actionsrunner.LambdaMicroVMAPI) (*actionsrunner.CleanupScheduleHandler, error) {
	if ctx == nil || secrets == nil || api == nil {
		return nil, errors.New("cleanup handler dependencies are required")
	}
	certPEM, err := readSecret(ctx, secrets, config.BrokerClientCertARN)
	if err != nil {
		return nil, errors.New("load Resource Plane broker certificate")
	}
	keyPEM, err := readSecret(ctx, secrets, config.BrokerClientKeyARN)
	if err != nil {
		wipeSecretBytes(certPEM)
		return nil, errors.New("load Resource Plane broker key")
	}
	caPEM, err := readSecret(ctx, secrets, config.BrokerCAARN)
	if err != nil {
		wipeSecretBytes(certPEM)
		wipeSecretBytes(keyPEM)
		return nil, errors.New("load Control Plane broker CA")
	}
	defer wipeSecretBytes(certPEM)
	defer wipeSecretBytes(keyPEM)
	defer wipeSecretBytes(caPEM)
	certificate, roots, err := runnerTLSFromPEM(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, errors.New("configure cleanup broker mTLS identity")
	}
	queueHTTP, err := githubbroker.NewActionsRunnerCleanupMTLSClient(config.ResourcePlaneID, certificate, roots)
	if err != nil {
		return nil, errors.New("configure cleanup queue mTLS client")
	}
	queue, err := githubbroker.NewHTTPActionsRunnerCleanupQueueClient(config.BrokerURL, config.ResourcePlaneID, queueHTTP)
	if err != nil {
		return nil, errors.New("configure cleanup queue broker")
	}
	deregisterHTTP, err := githubbroker.NewActionsRunnerCleanupMTLSClient(config.ResourcePlaneID, certificate, roots)
	if err != nil {
		return nil, errors.New("configure runner deregistration mTLS client")
	}
	deregistrator, err := githubbroker.NewHTTPActionsRunnerCleanupClient(config.BrokerURL, config.ResourcePlaneID, deregisterHTTP)
	if err != nil {
		return nil, errors.New("configure runner deregistration broker")
	}
	terminator, err := actionsrunner.NewSDKLambdaMicroVMTerminator(actionsrunner.LambdaTerminatorConfig{ResourcePlaneID: config.ResourcePlaneID,
		PollInterval: config.PollInterval, PollTimeout: config.PollTimeout}, api)
	if err != nil {
		return nil, errors.New("configure Lambda MicroVM cleanup adapter")
	}
	worker, err := actionsrunner.NewCleanupWorker(actionsrunner.CleanupWorkerConfig{ResourcePlaneID: config.ResourcePlaneID,
		WorkerID: "hako-runner-cleanup", LeaseDuration: config.LeaseDuration, OperationTimeout: config.OperationTimeout}, queue, deregistrator, terminator, time.Now)
	if err != nil {
		return nil, errors.New("configure Actions runner cleanup worker")
	}
	return actionsrunner.NewCleanupScheduleHandler(worker, config.BatchSize)
}

func boundedDuration(raw string, fallback, min, max time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value < min || value > max {
		return 0, errors.New("duration out of bounds")
	}
	return value, nil
}

func boundedInt(raw string, fallback, min, max int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < min || value > max {
		return 0, errors.New("integer out of bounds")
	}
	return value, nil
}
