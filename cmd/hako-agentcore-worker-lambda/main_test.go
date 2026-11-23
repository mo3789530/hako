package main

import (
	"testing"
)

func validEnvironment() map[string]string {
	return map[string]string{
		"HAKO_AGENTCORE_RESOURCE_PLANE_ID":      "rp-prod",
		"HAKO_AGENTCORE_BROKER_URL":             "https://broker.example.test",
		"HAKO_AGENTCORE_BROKER_CERT_SECRET_ARN": "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker-cert-AbCd",
		"HAKO_AGENTCORE_BROKER_KEY_SECRET_ARN":  "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker-key-AbCd",
		"HAKO_AGENTCORE_BROKER_CA_SECRET_ARN":   "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker-ca-AbCd",
		"HAKO_AGENTCORE_JOURNAL_BUCKET":         "hako-journals-prod",
		"HAKO_AGENTCORE_JOURNAL_PREFIX":         "workloads/rp-prod",
		"HAKO_AGENTCORE_SOURCE_BUCKET":          "hako-sources-prod",
		"HAKO_AGENTCORE_SOURCE_PREFIX":          "source/rp-prod",
		"HAKO_AGENTCORE_ARTIFACT_BUCKET":        "hako-artifacts-prod",
		"HAKO_AGENTCORE_ARTIFACT_PREFIX":        "workloads/rp-prod",
		"HAKO_AGENTCORE_ARTIFACT_KMS_KEY_ID":    "arn:aws:kms:ap-northeast-1:123456789012:key/11111111-2222-3333-4444-555555555555",
		"HAKO_AGENTCORE_RESULT_QUEUE_URL":       "https://sqs.ap-northeast-1.amazonaws.com/123456789012/workload-results",
	}
}

func TestConfiguredDeploymentAcceptsPinnedAWSFreeConfiguration(t *testing.T) {
	environment := validEnvironment()
	config, err := configuredDeployment(func(name string) string { return environment[name] })
	if err != nil || config.ResourcePlaneID != "rp-prod" || config.JournalBucket != "hako-journals-prod" ||
		config.ResultQueueURL != environment["HAKO_AGENTCORE_RESULT_QUEUE_URL"] {
		t.Fatalf("configured deployment=%+v err=%v", config, err)
	}
}

func TestConfiguredDeploymentRejectsUnsafeTargetsAndSecrets(t *testing.T) {
	for name, value := range map[string]string{
		"HAKO_AGENTCORE_BROKER_URL":            "http://broker.example.test",
		"HAKO_AGENTCORE_JOURNAL_BUCKET":        "../journal",
		"HAKO_AGENTCORE_SOURCE_PREFIX":         "source/../other",
		"HAKO_AGENTCORE_RESULT_QUEUE_URL":      "http://sqs.example.test/queue",
		"HAKO_AGENTCORE_BROKER_KEY_SECRET_ARN": "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker-cert-AbCd",
	} {
		t.Run(name, func(t *testing.T) {
			environment := validEnvironment()
			environment[name] = value
			if _, err := configuredDeployment(func(key string) string { return environment[key] }); err == nil {
				t.Fatalf("unsafe deployment setting %s was accepted", name)
			}
		})
	}
}
