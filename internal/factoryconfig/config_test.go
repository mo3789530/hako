package factoryconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestParseValidManifestNormalizesBoundedDefaults(t *testing.T) {
	config, err := Parse([]byte(`version: 1
pipelines:
  verify:
    steps: [checkout, go-test, go-vet]
    services: [postgres, redis]
    network: registry-only
`))
	if err != nil {
		t.Fatal(err)
	}
	pipeline := config.Pipelines["verify"]
	if config.Version != 1 || pipeline.RuntimeClass != "standard" || pipeline.TimeoutSecond != 1800 ||
		pipeline.MemoryMB != 2048 || pipeline.CPUMillis != 1000 || pipeline.Repository != "read" || pipeline.Network != "registry-only" {
		t.Fatalf("normalized manifest = %+v", config)
	}
}

func TestParseRejectsUnsafeOrAmbiguousManifest(t *testing.T) {
	base := manifest("    steps: [checkout]\n")
	cases := []struct {
		name string
		yaml string
	}{
		{name: "unsupported version", yaml: strings.Replace(base, "version: 1", "version: 2", 1)},
		{name: "unknown field", yaml: manifest("    steps: [checkout]\n    run: id\n")},
		{name: "arbitrary shell", yaml: manifest("    steps: [checkout, 'go-test; touch /tmp/pwn']\n")},
		{name: "unsupported image-like step", yaml: manifest("    steps: [checkout, 'docker:latest']\n")},
		{name: "write permission", yaml: manifest("    steps: [checkout]\n    repository: write\n")},
		{name: "cloud role", yaml: manifest("    steps: [checkout]\n    cloud_access: production\n")},
		{name: "public egress", yaml: manifest("    steps: [checkout]\n    network: internet\n")},
		{name: "untrusted service image", yaml: manifest("    steps: [checkout]\n    services: [custom-image]\n")},
		{name: "checkout is not first", yaml: manifest("    steps: [go-test, checkout]\n")},
		{name: "duplicate key", yaml: "version: 1\npipelines:\n  verify:\n    steps: [checkout]\n    steps: [go-test]\n"},
		{name: "anchor", yaml: "version: &version 1\npipelines: {verify: {steps: [checkout]}}"},
		{name: "alias", yaml: "steps: &steps [checkout]\nversion: 1\npipelines: {verify: {steps: *steps}}"},
		{name: "multiple documents", yaml: "version: 1\npipelines: {verify: {steps: [checkout]}}\n---\nversion: 1\npipelines: {other: {steps: [checkout]}}"},
		{name: "unknown version shape", yaml: strings.Replace(base, "version: 1", `version: "1"`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.yaml)); !errors.Is(err, ErrManifest) {
				t.Fatalf("Parse() error = %v, want ErrManifest", err)
			}
		})
	}
}

func TestParseRejectsManifestAndResourceBounds(t *testing.T) {
	if _, err := Parse(nil); !errors.Is(err, ErrManifest) {
		t.Fatalf("Parse(empty) error = %v", err)
	}
	if _, err := Parse([]byte(strings.Repeat("x", MaxManifestBytes+1))); !errors.Is(err, ErrManifest) {
		t.Fatalf("Parse(oversized) error = %v", err)
	}
	for _, yaml := range []string{
		manifest("    steps: [checkout]\n    timeout_seconds: 3601\n"),
		manifest("    steps: [checkout]\n    memory_mb: 9000\n"),
		manifest("    steps: [checkout]\n    cpu_millis: 5000\n"),
		manifest("    steps: [checkout]\n    runtime_class: large\n"),
		manifest("    steps: [checkout, checkout]\n"),
		strings.Replace(manifest("    steps: [checkout]\n"), "verify:", "Verify:", 1),
	} {
		if _, err := Parse([]byte(yaml)); !errors.Is(err, ErrManifest) {
			t.Errorf("Parse(%q) error = %v, want ErrManifest", yaml, err)
		}
	}
}

func manifest(pipelineFields string) string {
	return "version: 1\npipelines:\n  verify:\n" + pipelineFields
}

func TestParseRejectsExcessiveYAMLDepth(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("version: 1\npipelines:\n  verify:\n    steps: [checkout]\n    extra:")
	for i := 0; i < MaxYAMLDepth+2; i++ {
		builder.WriteString("\n      nested:")
	}
	builder.WriteString(" true\n")
	if _, err := Parse([]byte(builder.String())); !errors.Is(err, ErrManifest) {
		t.Fatalf("deep YAML error = %v, want ErrManifest", err)
	}
}
