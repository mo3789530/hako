package resourceplane

import (
	"strings"
	"testing"
)

const validManifest = `{
  "schema_version": 1,
  "resource_planes": [{
    "id": "rp-tokyo-01",
    "provider": "aws",
    "account_id": "123456789012",
    "region": "ap-northeast-1",
    "capabilities": ["microvm", "container", "private-network", "persistent-volume"],
    "command_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-commands",
    "result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"
  }]
}`

func TestParseResourcePlaneManifest(t *testing.T) {
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || len(manifest.ResourcePlanes) != 1 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	plane := manifest.ResourcePlanes[0]
	if plane.ID != "rp-tokyo-01" || plane.AccountID != "123456789012" || plane.Region != "ap-northeast-1" || len(plane.Capabilities) != 4 {
		t.Fatalf("unexpected registration: %+v", plane)
	}
}

func TestParseNormalizesCapabilitiesAndRejectsInvalidManifests(t *testing.T) {
	valid := strings.Replace(validManifest, `"microvm"`, `" MicroVM "`, 1)
	manifest, err := Parse([]byte(valid))
	if err != nil || manifest.ResourcePlanes[0].Capabilities[0] != "microvm" {
		t.Fatalf("normalized capability = %+v, %v", manifest, err)
	}
	tests := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"unsupported schema", strings.Replace(validManifest, `"schema_version": 1`, `"schema_version": 2`, 1)},
		{"unknown field", strings.Replace(validManifest, `"provider": "aws"`, `"provider": "aws", "typo": true`, 1)},
		{"trailing json", validManifest + `{}`},
		{"duplicate plane", strings.Replace(validManifest, `}]`, `}, {"id":"rp-tokyo-01"}]`, 1)},
		{"invalid id", strings.Replace(validManifest, `"rp-tokyo-01"`, `"RP Tokyo"`, 1)},
		{"unsupported provider", strings.Replace(validManifest, `"provider": "aws"`, `"provider": "azure"`, 1)},
		{"invalid account", strings.Replace(validManifest, `"123456789012"`, `"123"`, 1)},
		{"missing required microvm", strings.Replace(validManifest, `"microvm", `, ``, 1)},
		{"unknown capability", strings.Replace(validManifest, `"container"`, `"gpu"`, 1)},
		{"duplicate capability", strings.Replace(validManifest, `"container"`, `"microvm"`, 1)},
		{"unsafe queue url", strings.Replace(validManifest, "https://sqs.ap-northeast-1.amazonaws.com", "http://sqs.ap-northeast-1.amazonaws.com", 1)},
		{"lookalike queue host", strings.Replace(validManifest, "sqs.ap-northeast-1.amazonaws.com", "sqs.ap-northeast-1.amazonaws.com.attacker.test", 1)},
		{"queue host port", strings.Replace(validManifest, "sqs.ap-northeast-1.amazonaws.com/", "sqs.ap-northeast-1.amazonaws.com:8443/", 1)},
		{"wrong queue region", strings.Replace(validManifest, "sqs.ap-northeast-1", "sqs.us-east-1", 1)},
		{"wrong queue account", strings.Replace(validManifest, "/123456789012/", "/999999999999/", 1)},
		{"same queue url", strings.Replace(validManifest, "rp-tokyo-01-results", "rp-tokyo-01-commands", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse([]byte(test.data)); err == nil {
				t.Fatal("invalid manifest should be rejected")
			}
		})
	}
}

func TestParseBoundsManifestSize(t *testing.T) {
	if _, err := Parse([]byte(strings.Repeat(" ", MaxManifestBytes+1))); err == nil {
		t.Fatal("oversized manifest should be rejected")
	}
}

func TestManifestRejectsQueueURLReuseAcrossPlanes(t *testing.T) {
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	second := manifest.ResourcePlanes[0]
	second.ID = "rp-tokyo-02"
	manifest.ResourcePlanes = append(manifest.ResourcePlanes, second)
	if err := manifest.Validate(); err == nil {
		t.Fatal("a queue URL must not be shared by multiple Resource Planes")
	}
}

func TestManifestAcceptsSeparateWorkloadQueues(t *testing.T) {
	data := strings.Replace(validManifest,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"`,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results",
    "workload_command_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-commands",
    "workload_result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-results"`, 1)
	manifest, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.WorkloadCommandQueueURLs()["rp-tokyo-01"]; got != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-commands" {
		t.Fatalf("Workload command queue = %q", got)
	}
	if got := manifest.WorkloadResultQueueURLs()["rp-tokyo-01"]; got != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-results" {
		t.Fatalf("Workload result queue = %q", got)
	}
}

func TestManifestAcceptsSingleActionsRunnerStartupQueue(t *testing.T) {
	data := strings.Replace(validManifest,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"`,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results",
    "actions_runner_startup_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-actions-runner-startup"`, 1)
	manifest, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	registration, found, err := manifest.ActionsRunnerStartupQueue()
	if err != nil || !found || registration.ID != "rp-tokyo-01" || registration.Region != "ap-northeast-1" ||
		registration.ActionsRunnerStartupQueueURL != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-actions-runner-startup" {
		t.Fatalf("Actions runner startup queue = %+v found=%v error=%v", registration, found, err)
	}
}

func TestManifestAcceptsOptionalWorkspaceServiceQueue(t *testing.T) {
	data := strings.Replace(validManifest,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"`,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results",
    "workspace_service_command_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workspace-services"`, 1)
	manifest, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.WorkspaceServiceCommandQueueURLs()["rp-tokyo-01"]; got != "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workspace-services" {
		t.Fatalf("Workspace Service queue = %q", got)
	}
	duplicate := strings.Replace(data, "rp-tokyo-01-workspace-services", "rp-tokyo-01-commands", 1)
	if _, err := Parse([]byte(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicates another queue") {
		t.Fatalf("Workspace Service queue must not reuse another queue URL, err=%v", err)
	}
}

func TestManifestRejectsDuplicateAndMultipleActionsRunnerQueueRoutes(t *testing.T) {
	data := strings.Replace(validManifest,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"`,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results",
    "actions_runner_startup_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-commands"`, 1)
	if _, err := Parse([]byte(data)); err == nil || !strings.Contains(err.Error(), "duplicates another queue") {
		t.Fatalf("Actions runner queue reused a command queue: %v", err)
	}
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	first := manifest.ResourcePlanes[0]
	first.ActionsRunnerStartupQueueURL = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-actions-runner-startup"
	manifest.ResourcePlanes[0] = first
	second := first
	second.ID = "rp-tokyo-02"
	second.AccountID = "999999999999"
	second.CommandQueueURL = "https://sqs.ap-northeast-1.amazonaws.com/999999999999/rp-tokyo-02-commands"
	second.ResultQueueURL = "https://sqs.ap-northeast-1.amazonaws.com/999999999999/rp-tokyo-02-results"
	second.ActionsRunnerStartupQueueURL = "https://sqs.ap-northeast-1.amazonaws.com/999999999999/rp-tokyo-02-actions-runner-startup"
	manifest.ResourcePlanes = append(manifest.ResourcePlanes, second)
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := manifest.ActionsRunnerStartupQueue(); err == nil || found {
		t.Fatalf("multiple routes should fail closed: found=%v err=%v", found, err)
	}
}

func TestManifestRequiresDistinctPairedWorkloadQueues(t *testing.T) {
	withCommand := strings.Replace(validManifest,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"`,
		`"result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results",
    "workload_command_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-commands"`, 1)
	if _, err := Parse([]byte(withCommand)); err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("one-sided Workload queue config should be rejected, err=%v", err)
	}
	withBoth := strings.Replace(withCommand,
		`"workload_command_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-commands"`,
		`"workload_command_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-commands",
    "workload_result_queue_url": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-workload-commands"`, 1)
	if _, err := Parse([]byte(withBoth)); err == nil || !strings.Contains(err.Error(), "duplicates another queue") {
		t.Fatalf("reused Workload queue URL should be rejected, err=%v", err)
	}
}
