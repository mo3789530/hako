package main

import (
	"context"
	"errors"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

var secretARNPattern = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]{1,512}$`)

type secretValueAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

func validSecretARN(value string) bool {
	return secretARNPattern.MatchString(value)
}

func readSecret(ctx context.Context, api secretValueAPI, arn string, maxBytes int) ([]byte, error) {
	if ctx == nil || api == nil || !validSecretARN(arn) || maxBytes < 1 {
		return nil, errors.New("invalid Secret reference")
	}
	output, err := api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &arn})
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
		return nil, errors.New("Secret value is empty or oversized")
	}
	return value, nil
}
