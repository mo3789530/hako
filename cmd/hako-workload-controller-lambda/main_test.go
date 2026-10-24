package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadcontroller"
)

const testProfile = `[{"runtime_class":"standard","trust_level":"untrusted","image_identifier":"arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard","image_version":"1","image_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ingress_connector":"arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS","egress_connector":"arn:aws:lambda:ap-northeast-1:123456789012:network-connector:hako-egress"}]`

func TestParseProfiles(t *testing.T) {
	profiles, err := parseProfiles(testProfile)
	if err != nil {
		t.Fatal(err)
	}
	key := workloadcontroller.RuntimeProfileKey{RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted}
	if profile, ok := profiles[key]; !ok || profile.ImageVersion != "1" || len(profile.IngressNetworkConnectors) != 1 {
		t.Fatalf("unexpected parsed profile: %#v", profiles)
	}
}

func TestParseProfilesRejectsUnknownDuplicateAndUnsupportedProfiles(t *testing.T) {
	tests := map[string]string{
		"empty":             "",
		"unknown field":     strings.Replace(testProfile, `"runtime_class"`, `"extra":true,"runtime_class"`, 1),
		"duplicate":         strings.TrimSuffix(testProfile, "]") + "," + strings.TrimPrefix(testProfile, "["),
		"unsupported trust": strings.Replace(testProfile, `"untrusted"`, `"privileged-ish"`, 1),
		"trailing value":    testProfile + `{}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProfiles(input); err == nil {
				t.Fatal("invalid runtime profiles were accepted")
			}
		})
	}
	if _, err := parseProfiles(strings.Repeat(" ", maxProfileConfigBytes+1)); err == nil {
		t.Fatal("oversized runtime profile configuration was accepted")
	}
}

func TestConfiguredDeploymentRequiresCompleteModeSpecificSettings(t *testing.T) {
	values := baseEnvironment()
	cfg, err := configuredDeployment(func(key string) string { return values[key] })
	if err != nil || cfg.Mode != "command" || cfg.Runtime.ImageVersion != "1" {
		t.Fatalf("configured command deployment = %+v, err %v", cfg, err)
	}
	values["HAKO_WORKLOAD_LAMBDA_MODE"] = "recovery"
	if _, err := configuredDeployment(func(key string) string { return values[key] }); err == nil {
		t.Fatal("recovery mode without lease config was accepted")
	}
	values["HAKO_WORKLOAD_RECOVERY_LEASE_KEY"] = "runtime/leases/workload-sweep.json"
	values["HAKO_WORKLOAD_RECOVERY_PAGE_SIZE"] = "40"
	values["HAKO_WORKLOAD_RECOVERY_MAX_PAGES"] = "4"
	values["HAKO_WORKLOAD_RECOVERY_LEASE_TTL"] = "3m"
	cfg, err = configuredDeployment(func(key string) string { return values[key] })
	if err != nil || cfg.Recovery.PageSize != 40 || cfg.Recovery.MaxPages != 4 || cfg.Recovery.LeaseTTL != 3*time.Minute {
		t.Fatalf("configured recovery deployment = %+v, err %v", cfg, err)
	}
	values["HAKO_WORKLOAD_LAMBDA_MODE"] = "combined"
	cfg, err = configuredDeployment(func(key string) string { return values[key] })
	if err != nil || cfg.Mode != "combined" || cfg.Recovery.PageSize != 40 {
		t.Fatalf("configured combined command/recovery deployment = %+v, err %v", cfg, err)
	}
	values["HAKO_WORKLOAD_RECOVERY_PAGE_SIZE"] = "40garbage"
	if _, err := configuredDeployment(func(key string) string { return values[key] }); err == nil {
		t.Fatal("non-numeric recovery page size was accepted")
	}
	values["HAKO_WORKLOAD_RECOVERY_PAGE_SIZE"] = "40"
	values["HAKO_WORKLOAD_ARTIFACT_CLEANUP_BATCH_SIZE"] = "1001"
	if _, err := configuredDeployment(func(key string) string { return values[key] }); err == nil {
		t.Fatal("oversized artifact cleanup batch was accepted")
	}
}

func TestConfiguredDeploymentRejectsProfileRuntimeDrift(t *testing.T) {
	values := baseEnvironment()
	second := strings.Replace(testProfile, `"untrusted"`, `"trusted"`, 1)
	second = strings.Replace(second, "hako-standard", "hako-different", 1)
	values["HAKO_WORKLOAD_RUNTIME_PROFILES_JSON"] = strings.TrimSuffix(testProfile, "]") + "," + strings.Trim(second, "[]") + "]"
	if _, err := configuredDeployment(func(key string) string { return values[key] }); err == nil {
		t.Fatal("per-profile image variation should be rejected by one immutable Lambda worker config")
	}
}

func TestConfiguredDeploymentRequiresDistinctConcreteSecretARNs(t *testing.T) {
	values := baseEnvironment()
	values["HAKO_WORKLOAD_BROKER_CA_SECRET_ARN"] = values["HAKO_WORKLOAD_BROKER_CLIENT_KEY_SECRET_ARN"]
	if _, err := configuredDeployment(func(key string) string { return values[key] }); err == nil {
		t.Fatal("reused secret reference was accepted")
	}
	values = baseEnvironment()
	values["HAKO_WORKLOAD_ARTIFACT_CLEANUP_HMAC_SECRET_ARN"] = values["HAKO_WORKLOAD_CONTROL_PLANE_HMAC_SECRET_ARN"]
	if _, err := configuredDeployment(func(key string) string { return values[key] }); err == nil {
		t.Fatal("cleanup HMAC Secret reuse was accepted")
	}
	values = baseEnvironment()
	values["HAKO_WORKLOAD_TICKET_PRIVATE_KEY_SECRET_ARN"] = "arn:aws:secretsmanager:*:123456789012:secret:hako/ticket"
	if _, err := configuredDeployment(func(key string) string { return values[key] }); err == nil {
		t.Fatal("wildcard Secret ARN was accepted")
	}
}

func baseEnvironment() map[string]string {
	return map[string]string{
		"HAKO_WORKLOAD_LAMBDA_MODE":                      "command",
		"HAKO_RESOURCE_PLANE_ID":                         "rp-tokyo-01",
		"HAKO_WORKLOAD_CONTROL_PLANE_ENDPOINT":           "https://api.hako.example",
		"HAKO_WORKLOAD_CONTROL_PLANE_HMAC_SECRET_ARN":    "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-dev/metadata-hmac-AbCdEf",
		"HAKO_WORKLOAD_ARTIFACT_CLEANUP_HMAC_SECRET_ARN": "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-dev/artifact-cleanup-hmac-AbCdEf",
		"HAKO_WORKLOAD_CHECKOUT_BROKER_URL":              "https://broker.hako.internal",
		"HAKO_WORKLOAD_BROKER_CLIENT_CERT_SECRET_ARN":    "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-dev/broker-client-cert-AbCdEf",
		"HAKO_WORKLOAD_BROKER_CLIENT_KEY_SECRET_ARN":     "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-dev/broker-client-key-AbCdEf",
		"HAKO_WORKLOAD_BROKER_CA_SECRET_ARN":             "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-dev/broker-ca-AbCdEf",
		"HAKO_WORKLOAD_JOURNAL_BUCKET":                   "hako-workload-journal",
		"HAKO_WORKLOAD_JOURNAL_PREFIX":                   "v1/attempt-journals",
		"HAKO_WORKLOAD_SOURCE_BUCKET":                    "hako-workload-source",
		"HAKO_WORKLOAD_SOURCE_PREFIX":                    "v1/source",
		"HAKO_WORKLOAD_ARTIFACT_BUCKET":                  "hako-workload-artifacts",
		"HAKO_WORKLOAD_ARTIFACT_PREFIX":                  "v1/artifacts",
		"HAKO_WORKLOAD_RESULT_QUEUE_URL":                 "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-results",
		"HAKO_WORKLOAD_TICKET_KEY_ID":                    "controller-v1",
		"HAKO_WORKLOAD_TICKET_PRIVATE_KEY_SECRET_ARN":    "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-dev/agent-ticket-AbCdEf",
		"HAKO_WORKLOAD_RUNTIME_PROFILES_JSON":            testProfile,
	}
}
