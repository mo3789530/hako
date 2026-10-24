package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func TestConfiguredPreviewSourceRequiresPinnedDependencies(t *testing.T) {
	env := map[string]string{
		"HAKO_PREVIEW_CHECKOUT_BROKER_URL":           "https://broker.hako.test",
		"HAKO_PREVIEW_BROKER_CLIENT_CERT_SECRET_ARN": "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:preview-cert-AbCdEf",
		"HAKO_PREVIEW_BROKER_CLIENT_KEY_SECRET_ARN":  "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:preview-key-AbCdEf",
		"HAKO_PREVIEW_BROKER_CA_SECRET_ARN":          "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:preview-ca-AbCdEf",
		"HAKO_PREVIEW_SOURCE_BUCKET":                 "hako-source-example",
		"HAKO_PREVIEW_SOURCE_PREFIX":                 "hako/previews",
		"HAKO_PREVIEW_SOURCE_KMS_KEY_ID":             "arn:aws:kms:ap-northeast-1:123456789012:key/01234567-89ab-cdef-0123-456789abcdef",
		"HAKO_PREVIEW_GATEWAY_ORIGIN":                "https://gateway.hako.test",
		"HAKO_PREVIEW_TICKET_KEY_ID":                 "rp-preview-v1",
		"HAKO_PREVIEW_TICKET_PRIVATE_KEY_SECRET_ARN": "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:preview-ticket-AbCdEf",
	}
	getenv := func(key string) string { return env[key] }
	if _, err := configuredPreviewSource(getenv); err != nil {
		t.Fatalf("valid Preview source config rejected: %v", err)
	}
	for _, change := range []struct {
		name string
		key  string
		val  string
	}{
		{name: "broker downgrade", key: "HAKO_PREVIEW_CHECKOUT_BROKER_URL", val: "http://broker.hako.test"},
		{name: "duplicate signing secret", key: "HAKO_PREVIEW_TICKET_PRIVATE_KEY_SECRET_ARN", val: env["HAKO_PREVIEW_BROKER_CA_SECRET_ARN"]},
		{name: "unsafe prefix", key: "HAKO_PREVIEW_SOURCE_PREFIX", val: "hako/../shared"},
		{name: "invalid key id", key: "HAKO_PREVIEW_TICKET_KEY_ID", val: "bad/key"},
	} {
		t.Run(change.name, func(t *testing.T) {
			env[change.key] = change.val
			if _, err := configuredPreviewSource(getenv); err == nil {
				t.Fatal("unsafe Preview source configuration was accepted")
			}
		})
		env[change.key] = map[string]string{
			"HAKO_PREVIEW_CHECKOUT_BROKER_URL":           "https://broker.hako.test",
			"HAKO_PREVIEW_TICKET_PRIVATE_KEY_SECRET_ARN": "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:preview-ticket-AbCdEf",
			"HAKO_PREVIEW_SOURCE_PREFIX":                 "hako/previews",
			"HAKO_PREVIEW_TICKET_KEY_ID":                 "rp-preview-v1",
		}[change.key]
	}
}

type previewSecretFake struct {
	output *secretsmanager.GetSecretValueOutput
	err    error
}

func (f previewSecretFake) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return f.output, f.err
}

func TestReadPreviewSecretBoundsAndRejectsAmbiguousResponses(t *testing.T) {
	const arn = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:preview-key-AbCdEf"
	value := "synthetic-secret"
	got, err := readPreviewSecret(context.Background(), previewSecretFake{output: &secretsmanager.GetSecretValueOutput{SecretString: &value}}, arn, 64)
	if err != nil || string(got) != value {
		t.Fatalf("read synthetic secret=%q err=%v", got, err)
	}
	clearPreviewSecret(got)
	if strings.Trim(string(got), "\x00") != "" {
		t.Fatal("secret byte slice was not cleared")
	}
	for name, fake := range map[string]previewSecretFake{
		"api error": {err: errors.New("synthetic error")},
		"empty":     {output: &secretsmanager.GetSecretValueOutput{}},
		"ambiguous": {output: &secretsmanager.GetSecretValueOutput{SecretString: &value, SecretBinary: []byte("also-secret")}},
		"oversized": {output: &secretsmanager.GetSecretValueOutput{SecretString: &value}},
	} {
		t.Run(name, func(t *testing.T) {
			limit := 64
			if name == "oversized" {
				limit = 2
			}
			if _, err := readPreviewSecret(context.Background(), fake, arn, limit); err == nil {
				t.Fatal("invalid Secret Manager response was accepted")
			}
		})
	}
}
