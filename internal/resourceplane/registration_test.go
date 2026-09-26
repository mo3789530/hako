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
