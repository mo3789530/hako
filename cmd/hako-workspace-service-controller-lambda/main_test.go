package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func TestLoadDeploymentConfigRequiresAllSecurityAndRuntimeInputs(t *testing.T) {
	values := map[string]string{
		"HAKO_RESOURCE_PLANE_ID": "rp_test", "AWS_REGION": "us-east-1",
		"HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET": "hako-rp-journal", "HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX": "workspace",
		"HAKO_WORKSPACE_SERVICE_TICKET_KEY_ID":         "service-key",
		"HAKO_WORKSPACE_SERVICE_TICKET_KEY_SECRET_ARN": "arn:aws:secretsmanager:us-east-1:123456789012:secret:service-ticket-AbCdEf",
		"HAKO_WORKSPACE_SERVICE_REPORT_ENDPOINT":       "https://api.hako.example",
		"HAKO_WORKSPACE_SERVICE_REPORT_KEY_SECRET_ARN": "arn:aws:secretsmanager:us-east-1:123456789012:secret:service-report-AbCdEf",
	}
	getenv := func(key string) string { return values[key] }
	if _, err := loadDeploymentConfig(getenv); err != nil {
		t.Fatalf("valid deployment settings rejected: %v", err)
	}
	for _, key := range []string{"HAKO_RESOURCE_PLANE_ID", "AWS_REGION", "HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET",
		"HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX", "HAKO_WORKSPACE_SERVICE_TICKET_KEY_ID",
		"HAKO_WORKSPACE_SERVICE_TICKET_KEY_SECRET_ARN", "HAKO_WORKSPACE_SERVICE_REPORT_ENDPOINT", "HAKO_WORKSPACE_SERVICE_REPORT_KEY_SECRET_ARN"} {
		t.Run(key, func(t *testing.T) {
			copy := make(map[string]string, len(values))
			for name, value := range values {
				copy[name] = value
			}
			copy[key] = ""
			if _, err := loadDeploymentConfig(func(name string) string { return copy[name] }); err == nil {
				t.Fatalf("missing %s must fail closed", key)
			}
		})
	}
	if _, err := loadDeploymentConfig(nil); err == nil {
		t.Fatal("nil environment reader must be rejected")
	}
}

func TestParseEd25519PrivateKeyAndLoadSecretValue(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	loaded, err := parseEd25519PrivateKey(keyPEM)
	if err != nil || len(loaded) != ed25519.PrivateKeySize {
		t.Fatalf("parse valid Ed25519 key: key length=%d error=%v", len(loaded), err)
	}
	if _, err := parseEd25519PrivateKey([]byte("not a pem key")); err == nil {
		t.Fatal("invalid private key must fail closed")
	}
	const arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:service-report-AbCdEf"
	client := &secretValueFake{value: []byte("0123456789abcdef0123456789abcdef")}
	secret, err := getSecretValue(context.Background(), client, arn)
	if err != nil || string(secret) != string(client.value) || client.requestedARN != arn {
		t.Fatalf("read synthetic Secret value: value length=%d requested=%q error=%v", len(secret), client.requestedARN, err)
	}
	if _, err := getSecretValue(context.Background(), client, "not-an-arn"); err == nil {
		t.Fatal("invalid Secret ARN must fail before API call")
	}
	if _, err := getSecretValue(context.Background(), &secretValueFake{err: errors.New("denied")}, arn); err == nil {
		t.Fatal("Secrets Manager authorization failure must fail closed")
	}
	if _, err := getSecretValue(context.Background(), &secretValueFake{}, arn); err == nil {
		t.Fatal("empty Secret value must fail closed")
	}
}

type secretValueFake struct {
	value        []byte
	requestedARN string
	err          error
}

func (f *secretValueFake) GetSecretValue(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.requestedARN = aws.ToString(input.SecretId)
	return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(string(f.value))}, nil
}
