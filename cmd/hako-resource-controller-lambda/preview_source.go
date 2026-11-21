package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/previewcontroller"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadsource"
	"github.com/mo3789530/hako/internal/workspaceagent"
)

type previewSourceConfig struct {
	BrokerURL      string
	BrokerCertARN  string
	BrokerKeyARN   string
	BrokerCAARN    string
	SourceBucket   string
	SourcePrefix   string
	SourceKMSKeyID string
	GatewayOrigin  string
	TicketKeyID    string
	TicketKeyARN   string
}

var previewTicketKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type previewSecretAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

func configuredPreviewSource(getenv func(string) string) (previewSourceConfig, error) {
	if getenv == nil {
		return previewSourceConfig{}, errors.New("Preview source environment is unavailable")
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	config := previewSourceConfig{
		BrokerURL: get("HAKO_PREVIEW_CHECKOUT_BROKER_URL"), BrokerCertARN: get("HAKO_PREVIEW_BROKER_CLIENT_CERT_SECRET_ARN"),
		BrokerKeyARN: get("HAKO_PREVIEW_BROKER_CLIENT_KEY_SECRET_ARN"), BrokerCAARN: get("HAKO_PREVIEW_BROKER_CA_SECRET_ARN"),
		SourceBucket: get("HAKO_PREVIEW_SOURCE_BUCKET"), SourcePrefix: get("HAKO_PREVIEW_SOURCE_PREFIX"),
		SourceKMSKeyID: get("HAKO_PREVIEW_SOURCE_KMS_KEY_ID"), GatewayOrigin: get("HAKO_PREVIEW_GATEWAY_ORIGIN"),
		TicketKeyID: get("HAKO_PREVIEW_TICKET_KEY_ID"), TicketKeyARN: get("HAKO_PREVIEW_TICKET_PRIVATE_KEY_SECRET_ARN"),
	}
	endpoint, err := url.Parse(config.BrokerURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" {
		return previewSourceConfig{}, errors.New("Preview checkout broker must be an HTTPS origin")
	}
	origin, err := url.Parse(config.GatewayOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" {
		return previewSourceConfig{}, errors.New("Preview Gateway origin must be an HTTPS origin")
	}
	if config.BrokerCertARN == "" || config.BrokerKeyARN == "" || config.BrokerCAARN == "" || config.TicketKeyARN == "" ||
		!validResourcePlaneSecretARN(config.BrokerCertARN) || !validResourcePlaneSecretARN(config.BrokerKeyARN) ||
		!validResourcePlaneSecretARN(config.BrokerCAARN) || !validResourcePlaneSecretARN(config.TicketKeyARN) ||
		config.BrokerCertARN == config.BrokerKeyARN || config.BrokerCertARN == config.BrokerCAARN || config.BrokerCertARN == config.TicketKeyARN ||
		config.BrokerKeyARN == config.BrokerCAARN || config.BrokerKeyARN == config.TicketKeyARN || config.BrokerCAARN == config.TicketKeyARN ||
		config.SourceBucket == "" || !validPreviewSourcePrefix(config.SourcePrefix) || !previewTicketKeyIDPattern.MatchString(config.TicketKeyID) {
		return previewSourceConfig{}, errors.New("Preview source broker, archive, and signing key settings are incomplete")
	}
	return config, nil
}

func composePreviewSource(ctx context.Context, awsCfg aws.Config, plane domain.ResourcePlaneID, config previewSourceConfig) (
	*previewcontroller.PreviewSourceMaterializer, *previewcontroller.LambdaPreviewSourceInstaller, error) {
	if ctx == nil || plane == "" {
		return nil, nil, errors.New("Preview source composition identity is required")
	}
	secretAPI := secretsmanager.NewFromConfig(awsCfg)
	certPEM, err := readPreviewSecret(ctx, secretAPI, config.BrokerCertARN, 64<<10)
	if err != nil {
		return nil, nil, errors.New("load Preview checkout broker certificate")
	}
	keyPEM, err := readPreviewSecret(ctx, secretAPI, config.BrokerKeyARN, 64<<10)
	if err != nil {
		clearPreviewSecret(certPEM)
		return nil, nil, errors.New("load Preview checkout broker private key")
	}
	defer clearPreviewSecret(keyPEM)
	caPEM, err := readPreviewSecret(ctx, secretAPI, config.BrokerCAARN, 64<<10)
	if err != nil {
		clearPreviewSecret(certPEM)
		return nil, nil, errors.New("load Preview checkout broker CA")
	}
	certificate, roots, err := previewCheckoutTLS(certPEM, keyPEM, caPEM)
	clearPreviewSecret(certPEM)
	clearPreviewSecret(caPEM)
	if err != nil {
		return nil, nil, errors.New("configure Preview checkout broker mTLS")
	}
	brokerClient, err := githubbroker.NewResourcePlaneCheckoutMTLSClient(string(plane), certificate, roots)
	if err != nil {
		return nil, nil, errors.New("configure Preview checkout broker client")
	}
	checkoutProvider, err := githubbroker.NewHTTPPreviewCheckoutProvider(config.BrokerURL, brokerClient)
	if err != nil {
		return nil, nil, errors.New("configure Preview checkout broker endpoint")
	}
	awsS3 := s3.NewFromConfig(awsCfg)
	stager, err := workloadsource.NewS3SourceArchiveStager(awsS3, config.SourceBucket, config.SourcePrefix, config.SourceKMSKeyID, 0)
	if err != nil {
		return nil, nil, errors.New("configure Preview source archive stager")
	}
	materializer, err := previewcontroller.NewPreviewSourceMaterializer(previewCheckoutCredentialAdapter{provider: checkoutProvider}, previewcontroller.GitPreviewRepositoryCheckout{}, stager)
	if err != nil {
		return nil, nil, errors.New("configure Preview source materializer")
	}
	archiveReader, err := workloadsource.NewS3SourceArchiveReader(awsS3, config.SourceBucket, config.SourcePrefix, 0)
	if err != nil {
		return nil, nil, errors.New("configure Preview source archive reader")
	}
	ticketPEM, err := readPreviewSecret(ctx, secretAPI, config.TicketKeyARN, 16<<10)
	if err != nil {
		return nil, nil, errors.New("load Preview Guest ticket signing key")
	}
	privateKey, err := workloadagent.LoadTicketSigningKey(bytes.NewReader(ticketPEM))
	clearPreviewSecret(ticketPEM)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, nil, errors.New("parse Preview Guest ticket signing key")
	}
	ticketIssuer := &workspaceagent.AgentTicketIssuer{KeyID: config.TicketKeyID, PrivateKey: privateKey, TTL: 2 * time.Minute}
	installer, err := previewcontroller.NewLambdaPreviewSourceInstaller(previewcontroller.LambdaPreviewSourceInstallerConfig{
		ResourcePlaneID: plane, Region: awsCfg.Region, GatewayOrigin: config.GatewayOrigin,
		MicroVMs: lambdamicrovms.NewFromConfig(awsCfg), Tokens: workspaceagent.LambdaMicroVMAuthTokenProvider{API: lambdamicrovms.NewFromConfig(awsCfg)},
		Tickets: ticketIssuer, Archives: archiveReader,
	})
	if err != nil {
		for index := range privateKey {
			privateKey[index] = 0
		}
		return nil, nil, errors.New("configure Preview Guest source installer")
	}
	return materializer, installer, nil
}

type previewCheckoutCredentialAdapter struct {
	provider *githubbroker.HTTPPreviewCheckoutProvider
}

func (a previewCheckoutCredentialAdapter) CheckoutPreview(ctx context.Context, command previewprotocol.Command) (string, githubapp.RepositoryAccess, error) {
	if a.provider == nil {
		return "", githubapp.RepositoryAccess{}, errors.New("Preview checkout provider is unavailable")
	}
	grant, err := a.provider.CheckoutPreviewToken(ctx, githubbroker.PreviewCheckoutIdentity{TenantID: domain.TenantID(command.TenantID),
		InstallationID: command.InstallationID, RepositoryID: command.RepositoryID, PullRequest: command.PullRequestNumber,
		Generation: command.Generation, ClaimID: command.ClaimID, ClaimAttempt: command.ClaimAttempt,
		HeadSHA: command.HeadSHA, ResourcePlaneID: domain.ResourcePlaneID(command.ResourcePlaneID)})
	if err != nil {
		return "", githubapp.RepositoryAccess{}, err
	}
	return grant.Token(), grant.Repository(), nil
}

func previewCheckoutTLS(certPEM, keyPEM, caPEM []byte) (tls.Certificate, *x509.CertPool, error) {
	if len(certPEM) == 0 || len(certPEM) > 64<<10 || len(keyPEM) == 0 || len(keyPEM) > 64<<10 || len(caPEM) == 0 || len(caPEM) > 64<<10 {
		return tls.Certificate{}, nil, errors.New("invalid Preview broker TLS material")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid Preview broker client certificate")
	}
	certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid Preview broker certificate leaf")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("invalid Preview broker CA bundle")
	}
	return certificate, roots, nil
}

func validResourcePlaneSecretARN(value string) bool {
	parts := strings.Split(value, ":")
	return len(parts) == 7 && (parts[1] == "aws" || parts[1] == "aws-us-gov" || parts[1] == "aws-cn") &&
		parts[2] == "secretsmanager" && parts[3] != "" && len(parts[4]) == 12 && parts[5] == "secret" && parts[6] != ""
}

func readPreviewSecret(ctx context.Context, api previewSecretAPI, arn string, maxBytes int) ([]byte, error) {
	if ctx == nil || api == nil || !validResourcePlaneSecretARN(arn) || maxBytes < 1 || maxBytes > 64<<10 {
		return nil, errors.New("invalid Resource Plane Secret request")
	}
	output, err := api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
	if err != nil || output == nil || (output.SecretString == nil) == (len(output.SecretBinary) == 0) {
		return nil, errors.New("Resource Plane Secret is unavailable")
	}
	var value []byte
	if output.SecretString != nil {
		value = []byte(*output.SecretString)
	} else {
		value = append([]byte(nil), output.SecretBinary...)
	}
	if len(value) == 0 || len(value) > maxBytes {
		clearPreviewSecret(value)
		return nil, errors.New("Resource Plane Secret is invalid")
	}
	return value, nil
}

func clearPreviewSecret(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func validPreviewSourcePrefix(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.HasPrefix(value, "/") && !strings.HasSuffix(value, "/") &&
		!strings.Contains(value, "..") && !strings.ContainsAny(value, "\\\x00")
}

var _ previewcontroller.PreviewSourceCredentialProvider = previewCheckoutCredentialAdapter{}
