package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

const validTestSecretARN = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-dev/example-AbCdEf"

type fakeSecretAPI struct {
	output *secretsmanager.GetSecretValueOutput
	err    error
	input  *secretsmanager.GetSecretValueInput
	calls  int
}

func (f *fakeSecretAPI) GetSecretValue(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.calls++
	f.input = input
	return f.output, f.err
}

func TestReadSecretUsesExactARNAndAcceptsStringOrBinary(t *testing.T) {
	stringSecret := "synthetic-secret-value"
	for name, output := range map[string]*secretsmanager.GetSecretValueOutput{
		"string": {SecretString: &stringSecret},
		"binary": {SecretBinary: []byte("synthetic-binary-secret")},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeSecretAPI{output: output}
			got, err := readSecret(context.Background(), api, validTestSecretARN, 128)
			if err != nil || len(got) == 0 || api.calls != 1 || api.input == nil || api.input.SecretId == nil || *api.input.SecretId != validTestSecretARN {
				t.Fatalf("readSecret = %q, %v; input=%+v calls=%d", got, err, api.input, api.calls)
			}
		})
	}
}

func TestReadSecretRejectsMalformedUnavailableAmbiguousAndOversizedValues(t *testing.T) {
	secret := "synthetic-secret-value"
	tests := map[string]struct {
		ctx     context.Context
		arn     string
		api     *fakeSecretAPI
		maxSize int
	}{
		"nil context": {ctx: nil, arn: validTestSecretARN, api: &fakeSecretAPI{output: &secretsmanager.GetSecretValueOutput{SecretString: &secret}}, maxSize: 128},
		"invalid arn": {ctx: context.Background(), arn: "*", api: &fakeSecretAPI{output: &secretsmanager.GetSecretValueOutput{SecretString: &secret}}, maxSize: 128},
		"api error":   {ctx: context.Background(), arn: validTestSecretARN, api: &fakeSecretAPI{err: errors.New("synthetic failure")}, maxSize: 128},
		"nil output":  {ctx: context.Background(), arn: validTestSecretARN, api: &fakeSecretAPI{}, maxSize: 128},
		"empty value": {ctx: context.Background(), arn: validTestSecretARN, api: &fakeSecretAPI{output: &secretsmanager.GetSecretValueOutput{SecretString: ptr("")}}, maxSize: 128},
		"both forms":  {ctx: context.Background(), arn: validTestSecretARN, api: &fakeSecretAPI{output: &secretsmanager.GetSecretValueOutput{SecretString: &secret, SecretBinary: []byte("also-secret")}}, maxSize: 128},
		"oversized":   {ctx: context.Background(), arn: validTestSecretARN, api: &fakeSecretAPI{output: &secretsmanager.GetSecretValueOutput{SecretString: &secret}}, maxSize: 4},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := readSecret(test.ctx, test.api, test.arn, test.maxSize)
			if err == nil || got != nil {
				t.Fatalf("readSecret = %q, %v; want fail-closed empty result", got, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("error included Secret material")
			}
		})
	}
}

func TestValidSecretARNRequiresConcreteSecretsManagerARN(t *testing.T) {
	for _, value := range []string{
		validTestSecretARN,
		"arn:aws-us-gov:secretsmanager:us-gov-west-1:123456789012:secret:tenant/app-key-AbCdEf",
		"arn:aws-cn:secretsmanager:cn-north-1:123456789012:secret:tenant/app-key-AbCdEf",
	} {
		if !validSecretARN(value) {
			t.Errorf("validSecretARN(%q) = false", value)
		}
	}
	for _, value := range []string{"", "*", "arn:aws:secretsmanager:*:*:secret:foo", "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:foo*"} {
		if validSecretARN(value) {
			t.Errorf("validSecretARN(%q) = true", value)
		}
	}
}

func ptr(value string) *string { return &value }
