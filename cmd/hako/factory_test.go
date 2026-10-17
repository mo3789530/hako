package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFactoryValidateCLI(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: nil, want: ".hako/factory.yaml"},
		{args: []string{"--file", "factory.yaml"}, want: "factory.yaml"},
	} {
		input, err := parseFactoryValidateCLI(test.args)
		if err != nil || input.path != test.want {
			t.Fatalf("parseFactoryValidateCLI(%v) = %+v, error=%v", test.args, input, err)
		}
	}
	for _, args := range [][]string{{"--file", " "}, {"manifest.yaml"}, {"--unknown"}} {
		if _, err := parseFactoryValidateCLI(args); err == nil {
			t.Errorf("parseFactoryValidateCLI(%v) unexpectedly succeeded", args)
		}
	}
}

func TestFactoryValidateCLIReportsOnlyValidatedPipelineNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "factory.yaml")
	manifest := `version: 1
pipelines:
  z-test:
    steps: [checkout, go-test]
  a-lint:
    steps: [checkout, golangci-lint]
`
	if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := validateFactoryManifestCLI([]string{"--file", path}, &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "Valid Hako Factory manifest v1 (2 pipelines): a-lint, z-test\n"; got != want {
		t.Fatalf("validation output=%q want=%q", got, want)
	}
}

func TestFactoryValidateCLIDoesNotEchoInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "factory.yaml")
	malicious := "version: 1\npipelines: {verify: {steps: [checkout, 'go-test; echo secret']}}\n"
	if err := os.WriteFile(path, []byte(malicious), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := validateFactoryManifestCLI([]string{"--file", path}, &output)
	if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "echo secret") {
		t.Fatalf("invalid manifest leaked details or succeeded: output=%q error=%v", output.String(), err)
	}
}
