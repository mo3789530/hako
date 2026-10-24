// hako-actions-runner-lambda is the Resource Plane entrypoint for one-shot
// GitHub Actions runner startup messages. It does not connect to Control Plane
// PostgreSQL; claims and tokens are mediated by the mTLS GitHub broker.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/mo3789530/hako/internal/actionsrunner"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/workloadagent"
)

const maxRunnerSecretBytes = 64 << 10

type deploymentConfig struct {
	ResourcePlaneID        string
	BrokerURL              string
	BrokerClientCertARN    string
	BrokerClientKeyARN     string
	BrokerCAARN            string
	BootstrapSigningKeyARN string
	ImageIdentifier        string
	ImageVersion           string
	ExecutionRoleARN       string
	EgressNetworkConnector string
	MaximumDuration        int32
	OperationTimeout       time.Duration
}

type secretAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type runnerLambdaAPI interface {
	actionsrunner.LambdaRunnerAPI
	actionsrunner.RunnerBootstrapAuthAPI
}

func main() {
	if err := run(); err != nil {
		log.Fatal("Hako Actions runner Lambda stopped (failure_class=process_exit)")
	}
}

func run() error {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("HAKO_ACTIONS_RUNNER_MODE")))
	if mode == "cleanup" {
		return runCleanup()
	}
	if mode != "" && mode != "startup" {
		return errors.New("HAKO_ACTIONS_RUNNER_MODE must be startup or cleanup")
	}
	config, err := configuredDeployment(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return errors.New("load Actions runner Resource Plane AWS configuration")
	}
	handler, err := buildHandler(ctx, config, secretsmanager.NewFromConfig(awsConfig), lambdamicrovms.NewFromConfig(awsConfig))
	if err != nil {
		return errors.New("compose Actions runner Resource Plane handler")
	}
	lambda.Start(handler.Handle)
	return nil
}

func configuredDeployment(getenv func(string) string) (deploymentConfig, error) {
	if getenv == nil {
		return deploymentConfig{}, errors.New("environment reader is required")
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	config := deploymentConfig{ResourcePlaneID: get("HAKO_RESOURCE_PLANE_ID"), BrokerURL: get("HAKO_ACTIONS_RUNNER_BROKER_URL"),
		BrokerClientCertARN: get("HAKO_ACTIONS_RUNNER_BROKER_CLIENT_CERT_SECRET_ARN"), BrokerClientKeyARN: get("HAKO_ACTIONS_RUNNER_BROKER_CLIENT_KEY_SECRET_ARN"),
		BrokerCAARN: get("HAKO_ACTIONS_RUNNER_BROKER_CA_SECRET_ARN"), BootstrapSigningKeyARN: get("HAKO_ACTIONS_RUNNER_BOOTSTRAP_SIGNING_KEY_SECRET_ARN"),
		ImageIdentifier: get("HAKO_ACTIONS_RUNNER_MICROVM_IMAGE_ARN"), ImageVersion: get("HAKO_ACTIONS_RUNNER_MICROVM_IMAGE_VERSION"),
		ExecutionRoleARN: get("HAKO_ACTIONS_RUNNER_EXECUTION_ROLE_ARN"), EgressNetworkConnector: get("HAKO_ACTIONS_RUNNER_EGRESS_CONNECTOR_ARN")}
	if config.ResourcePlaneID == "" || config.BrokerURL == "" || config.ImageIdentifier == "" || config.ImageVersion == "" || config.EgressNetworkConnector == "" ||
		!validSecretARN(config.BrokerClientCertARN) || !validSecretARN(config.BrokerClientKeyARN) || !validSecretARN(config.BrokerCAARN) ||
		!validSecretARN(config.BootstrapSigningKeyARN) {
		return deploymentConfig{}, errors.New("Resource Plane, HTTPS broker, runner image/network, and distinct Secret references are required")
	}
	parsedBroker, err := githubbroker.NewHTTPActionsRunnerLaunchClient(config.BrokerURL, config.ResourcePlaneID, &http.Client{Timeout: 15 * time.Second})
	if err != nil || parsedBroker == nil {
		return deploymentConfig{}, errors.New("HAKO_ACTIONS_RUNNER_BROKER_URL or Resource Plane identity is invalid")
	}
	arns := []string{config.BrokerClientCertARN, config.BrokerClientKeyARN, config.BrokerCAARN, config.BootstrapSigningKeyARN}
	seen := make(map[string]struct{}, len(arns))
	for _, arn := range arns {
		if _, duplicate := seen[arn]; duplicate {
			return deploymentConfig{}, errors.New("Actions runner broker and signing-key Secret references must be distinct")
		}
		seen[arn] = struct{}{}
	}
	config.MaximumDuration, err = positiveInt32(get("HAKO_ACTIONS_RUNNER_MAXIMUM_DURATION_SECONDS"), 3600, 1, 8*60*60)
	if err != nil {
		return deploymentConfig{}, errors.New("HAKO_ACTIONS_RUNNER_MAXIMUM_DURATION_SECONDS must be 1-28800")
	}
	config.OperationTimeout, err = time.ParseDuration(defaultString(get("HAKO_ACTIONS_RUNNER_STARTUP_TIMEOUT"), "4m"))
	if err != nil || config.OperationTimeout < 5*time.Second || config.OperationTimeout > 5*time.Minute {
		return deploymentConfig{}, errors.New("HAKO_ACTIONS_RUNNER_STARTUP_TIMEOUT must be 5s-5m")
	}
	return config, nil
}

func buildHandler(ctx context.Context, config deploymentConfig, secrets secretAPI, api runnerLambdaAPI) (*actionsrunner.RunnerStartupHandler, error) {
	if ctx == nil || secrets == nil || api == nil {
		return nil, errors.New("runner handler dependencies are required")
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
	signingPEM, err := readSecret(ctx, secrets, config.BootstrapSigningKeyARN)
	if err != nil {
		wipeSecretBytes(certPEM)
		wipeSecretBytes(keyPEM)
		wipeSecretBytes(caPEM)
		return nil, errors.New("load runner bootstrap signing key")
	}
	defer wipeSecretBytes(certPEM)
	defer wipeSecretBytes(keyPEM)
	defer wipeSecretBytes(caPEM)
	defer wipeSecretBytes(signingPEM)
	certificate, roots, err := runnerTLSFromPEM(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, errors.New("configure broker mTLS identity")
	}
	signingKey, err := workloadagent.LoadTicketSigningKey(bytes.NewReader(signingPEM))
	if err != nil {
		return nil, errors.New("parse runner bootstrap signing key")
	}
	defer wipeSecretBytes(signingKey)
	launchHTTP, err := githubbroker.NewActionsRunnerLaunchMTLSClient(config.ResourcePlaneID, certificate, roots)
	if err != nil {
		return nil, errors.New("configure runner launch mTLS client")
	}
	launchBroker, err := githubbroker.NewHTTPActionsRunnerLaunchClient(config.BrokerURL, config.ResourcePlaneID, launchHTTP)
	if err != nil {
		return nil, errors.New("configure runner launch broker")
	}
	bindingHTTP, err := githubbroker.NewActionsRunnerBindingMTLSClient(config.ResourcePlaneID, certificate, roots)
	if err != nil {
		return nil, errors.New("configure runner binding mTLS client")
	}
	binder, err := githubbroker.NewHTTPActionsRunnerBindingClient(config.BrokerURL, config.ResourcePlaneID, bindingHTTP)
	if err != nil {
		return nil, errors.New("configure runner claim binder")
	}
	tokenHTTP, err := githubbroker.NewActionsRunnerBootstrapTokenMTLSClient(config.ResourcePlaneID, certificate, roots)
	if err != nil {
		return nil, errors.New("configure runner bootstrap-token mTLS client")
	}
	tokenProvider, err := githubbroker.NewHTTPActionsRunnerBootstrapTokenClient(config.BrokerURL, tokenHTTP)
	if err != nil {
		return nil, errors.New("configure runner token broker")
	}
	failureHTTP, err := githubbroker.NewActionsRunnerStartupFailureMTLSClient(config.ResourcePlaneID, certificate, roots)
	if err != nil {
		return nil, errors.New("configure runner startup-failure mTLS client")
	}
	failureReporter, err := githubbroker.NewHTTPActionsRunnerStartupFailureClient(config.BrokerURL, failureHTTP)
	if err != nil {
		return nil, errors.New("configure runner startup-failure broker")
	}
	launcher, err := actionsrunner.NewLambdaRunnerLauncher(actionsrunner.LambdaRunnerLaunchConfig{ResourcePlaneID: config.ResourcePlaneID,
		ImageIdentifier: config.ImageIdentifier, ImageVersion: config.ImageVersion, ExecutionRoleARN: config.ExecutionRoleARN,
		EgressNetworkConnector: config.EgressNetworkConnector, MaximumDuration: config.MaximumDuration}, api)
	if err != nil {
		return nil, errors.New("configure trusted Lambda MicroVM runner profile")
	}
	bootstrapper, err := actionsrunner.NewRunnerBootstrapper(api, &http.Client{Timeout: 15 * time.Second}, signingKey)
	if err != nil {
		return nil, errors.New("configure signed runner bootstrap sender")
	}
	worker, err := actionsrunner.NewRunnerStartupWorker(actionsrunner.RunnerStartupWorkerConfig{OperationTimeout: config.OperationTimeout},
		launchBroker, launcher, binder, tokenProvider, bootstrapper, failureReporter, config.ResourcePlaneID)
	if err != nil {
		return nil, errors.New("configure runner startup worker")
	}
	return actionsrunner.NewRunnerStartupHandler(worker)
}

func readSecret(ctx context.Context, api secretAPI, arn string) ([]byte, error) {
	if ctx == nil || api == nil || !validSecretARN(arn) {
		return nil, errors.New("invalid Secret lookup")
	}
	output, err := api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
	if err != nil || output == nil {
		return nil, errors.New("read Secret")
	}
	if output.SecretString != nil {
		value := []byte(*output.SecretString)
		if len(value) == 0 || len(value) > maxRunnerSecretBytes {
			wipeSecretBytes(value)
			return nil, errors.New("invalid Secret size")
		}
		return value, nil
	}
	if len(output.SecretBinary) == 0 || len(output.SecretBinary) > maxRunnerSecretBytes {
		return nil, errors.New("invalid Secret size")
	}
	return append([]byte(nil), output.SecretBinary...), nil
}

func runnerTLSFromPEM(certPEM, keyPEM, caPEM []byte) (tls.Certificate, *x509.CertPool, error) {
	if len(certPEM) == 0 || len(certPEM) > maxRunnerSecretBytes || len(keyPEM) == 0 || len(keyPEM) > maxRunnerSecretBytes ||
		len(caPEM) == 0 || len(caPEM) > maxRunnerSecretBytes {
		return tls.Certificate{}, nil, errors.New("invalid broker certificate material")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid broker client key pair")
	}
	certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid broker client certificate")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("invalid broker CA bundle")
	}
	return certificate, roots, nil
}

func validSecretARN(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), ":")
	return len(parts) == 7 && parts[0] == "arn" && parts[1] == "aws" && parts[2] == "secretsmanager" && parts[3] != "" &&
		len(parts[4]) == 12 && parts[5] == "secret" && parts[6] != ""
}

func positiveInt32(raw string, fallback, min, max int32) (int32, error) {
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed < int64(min) || parsed > int64(max) {
		return 0, errors.New("invalid bounded integer")
	}
	return int32(parsed), nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func wipeSecretBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
