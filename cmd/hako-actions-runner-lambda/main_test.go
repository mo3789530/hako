package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	microvmtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

type fakeSecretAPI struct {
	values map[string][]byte
	calls  int
}

func (f *fakeSecretAPI) GetSecretValue(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.calls++
	value, ok := f.values[aws.ToString(input.SecretId)]
	if !ok {
		return nil, &types.ResourceNotFoundException{}
	}
	copyValue := append([]byte(nil), value...)
	return &secretsmanager.GetSecretValueOutput{SecretBinary: copyValue}, nil
}

type fakeRunnerLambdaAPI struct{}

func (fakeRunnerLambdaAPI) RunMicrovm(context.Context, *lambdamicrovms.RunMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error) {
	return nil, nil
}

type fakeCleanupLambdaAPI struct{}

func (fakeCleanupLambdaAPI) GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	return &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("unused"), State: microvmtypes.MicrovmStateTerminated}, nil
}
func (fakeCleanupLambdaAPI) TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}
func (fakeRunnerLambdaAPI) CreateMicrovmAuthToken(context.Context, *lambdamicrovms.CreateMicrovmAuthTokenInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	return nil, nil
}

func TestConfiguredDeploymentValidatesWorkerProfile(t *testing.T) {
	values := map[string]string{
		"HAKO_RESOURCE_PLANE_ID": "rp-tokyo-01", "HAKO_ACTIONS_RUNNER_BROKER_URL": "https://broker.example",
		"HAKO_ACTIONS_RUNNER_DIAGNOSTICS_UPLOAD_BASE_URL":      "https://api.hako.example",
		"HAKO_ACTIONS_RUNNER_BROKER_CLIENT_CERT_SECRET_ARN":    "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-cert",
		"HAKO_ACTIONS_RUNNER_BROKER_CLIENT_KEY_SECRET_ARN":     "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-key",
		"HAKO_ACTIONS_RUNNER_BROKER_CA_SECRET_ARN":             "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-ca",
		"HAKO_ACTIONS_RUNNER_BOOTSTRAP_SIGNING_KEY_SECRET_ARN": "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:bootstrap-signing",
		"HAKO_ACTIONS_RUNNER_MICROVM_IMAGE_ARN":                "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-runner",
		"HAKO_ACTIONS_RUNNER_MICROVM_IMAGE_VERSION":            "3",
		"HAKO_ACTIONS_RUNNER_EGRESS_CONNECTOR_ARN":             "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:restricted",
	}
	config, err := configuredDeployment(func(key string) string { return values[key] })
	if err != nil || config.MaximumDuration != 3600 || config.OperationTimeout != 4*time.Minute {
		t.Fatalf("configuredDeployment() = %+v, %v", config, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"missing signing key": func(env map[string]string) { delete(env, "HAKO_ACTIONS_RUNNER_BOOTSTRAP_SIGNING_KEY_SECRET_ARN") },
		"duplicate secrets": func(env map[string]string) {
			env["HAKO_ACTIONS_RUNNER_BROKER_CLIENT_KEY_SECRET_ARN"] = env["HAKO_ACTIONS_RUNNER_BROKER_CLIENT_CERT_SECRET_ARN"]
		},
		"unsafe broker URL":                func(env map[string]string) { env["HAKO_ACTIONS_RUNNER_BROKER_URL"] = "http://broker.example" },
		"runtime exceeds platform maximum": func(env map[string]string) { env["HAKO_ACTIONS_RUNNER_MAXIMUM_DURATION_SECONDS"] = "28801" },
	} {
		t.Run(name, func(t *testing.T) {
			copyEnv := make(map[string]string, len(values))
			for key, value := range values {
				copyEnv[key] = value
			}
			mutate(copyEnv)
			if _, err := configuredDeployment(func(key string) string { return copyEnv[key] }); err == nil {
				t.Fatal("invalid deployment configuration accepted")
			}
		})
	}
}

func TestBuildHandlerUsesFakeSecretsAndPlaneBoundMTLS(t *testing.T) {
	config := deploymentConfig{ResourcePlaneID: "rp-tokyo-01", BrokerURL: "https://broker.example",
		DiagnosticsUploadBaseURL: "https://api.hako.example",
		BrokerClientCertARN:      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-cert",
		BrokerClientKeyARN:       "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-key",
		BrokerCAARN:              "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-ca",
		BootstrapSigningKeyARN:   "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:bootstrap-signing",
		ImageIdentifier:          "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-runner", ImageVersion: "3",
		EgressNetworkConnector: "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:restricted", MaximumDuration: 3600, OperationTimeout: time.Minute}
	certPEM, keyPEM, caPEM := testBrokerCertificate(t, "spiffe://hako/resource-plane/rp-tokyo-01")
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	signingPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	secrets := &fakeSecretAPI{values: map[string][]byte{config.BrokerClientCertARN: certPEM, config.BrokerClientKeyARN: keyPEM,
		config.BrokerCAARN: caPEM, config.BootstrapSigningKeyARN: signingPEM}}
	handler, err := buildHandler(context.Background(), config, secrets, fakeRunnerLambdaAPI{})
	if err != nil || handler == nil || secrets.calls != 4 {
		t.Fatalf("buildHandler() handler=%v secretReads=%d error=%v", handler != nil, secrets.calls, err)
	}
}

func TestConfiguredCleanupDeploymentBoundsWorkerAndRequiresSeparateSecrets(t *testing.T) {
	values := map[string]string{
		"HAKO_RESOURCE_PLANE_ID": "rp-tokyo-01", "HAKO_ACTIONS_RUNNER_BROKER_URL": "https://broker.example",
		"HAKO_ACTIONS_RUNNER_BROKER_CLIENT_CERT_SECRET_ARN": "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-cert",
		"HAKO_ACTIONS_RUNNER_BROKER_CLIENT_KEY_SECRET_ARN":  "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-key",
		"HAKO_ACTIONS_RUNNER_BROKER_CA_SECRET_ARN":          "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-ca",
	}
	config, err := configuredCleanupDeployment(func(key string) string { return values[key] })
	if err != nil || config.BatchSize != 20 || config.LeaseDuration != 2*time.Minute || config.OperationTimeout != 90*time.Second {
		t.Fatalf("configuredCleanupDeployment() = %+v, %v", config, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"unsafe broker": func(env map[string]string) { env["HAKO_ACTIONS_RUNNER_BROKER_URL"] = "http://broker.example" },
		"duplicate secret": func(env map[string]string) {
			env["HAKO_ACTIONS_RUNNER_BROKER_CA_SECRET_ARN"] = env["HAKO_ACTIONS_RUNNER_BROKER_CLIENT_KEY_SECRET_ARN"]
		},
		"operation exceeds lease": func(env map[string]string) {
			env["HAKO_ACTIONS_RUNNER_CLEANUP_OPERATION_TIMEOUT"] = "2m"
		},
		"batch out of bounds":    func(env map[string]string) { env["HAKO_ACTIONS_RUNNER_CLEANUP_BATCH_SIZE"] = "101" },
		"poll exceeds operation": func(env map[string]string) { env["HAKO_ACTIONS_RUNNER_CLEANUP_POLL_TIMEOUT"] = "2m" },
	} {
		t.Run(name, func(t *testing.T) {
			copyEnv := make(map[string]string, len(values))
			for key, value := range values {
				copyEnv[key] = value
			}
			mutate(copyEnv)
			if _, err := configuredCleanupDeployment(func(key string) string { return copyEnv[key] }); err == nil {
				t.Fatal("invalid cleanup deployment configuration accepted")
			}
		})
	}
}

func TestBuildCleanupHandlerUsesFakeSecretsAndPlaneBoundClients(t *testing.T) {
	config := cleanupDeploymentConfig{ResourcePlaneID: "rp-tokyo-01", BrokerURL: "https://broker.example",
		BrokerClientCertARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-cert",
		BrokerClientKeyARN:  "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-key",
		BrokerCAARN:         "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:broker-ca",
		LeaseDuration:       2 * time.Minute, OperationTimeout: 90 * time.Second, PollInterval: 10 * time.Millisecond, PollTimeout: time.Second, BatchSize: 2}
	certPEM, keyPEM, caPEM := testBrokerCertificate(t, "spiffe://hako/resource-plane/rp-tokyo-01")
	secrets := &fakeSecretAPI{values: map[string][]byte{config.BrokerClientCertARN: certPEM, config.BrokerClientKeyARN: keyPEM, config.BrokerCAARN: caPEM}}
	handler, err := buildCleanupHandler(context.Background(), config, secrets, fakeCleanupLambdaAPI{})
	if err != nil || handler == nil || secrets.calls != 3 {
		t.Fatalf("buildCleanupHandler() handler=%v secretReads=%d error=%v", handler != nil, secrets.calls, err)
	}
}

func testBrokerCertificate(t *testing.T, spiffeID string) ([]byte, []byte, []byte) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test broker root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spiffeURL, err := url.Parse(spiffeID)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "runner resource plane"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{spiffeURL}}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, rootCert, &clientKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPKCS8, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: clientPKCS8}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
}
