package workloadagent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type countingVerifierResourceLimiter struct{ calls int }

func (l *countingVerifierResourceLimiter) Open(context.Context, int, int) (JobProcessResourceScope, error) {
	l.calls++
	return nil, errors.New("unexpected resource scope")
}

func TestNativeAgentGoProfileRequiresGoOnlyModule(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/project\n\ngo 1.24\n")
	write("main_test.go", "package project\n")
	if err := requireNativeAgentGoProfile(root); err != nil {
		t.Fatalf("Go-only module rejected: %v", err)
	}
	for _, name := range []string{"src/main.py", "services/api.ts", "src/Main.java", "crates/app.rs", "infra/main.tf", "terraform.tfstate.backup"} {
		t.Run(name, func(t *testing.T) {
			write(name, "untrusted source\n")
			if err := requireNativeAgentGoProfile(root); !errors.Is(err, ErrVerifierProfileUnavailable) {
				t.Fatalf("Go-only profile accepted unsupported source %q: %v", name, err)
			}
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeAgentGoProfileRejectsMissingModuleOrSources(t *testing.T) {
	for _, setup := range []func(string) error{
		func(string) error { return nil },
		func(root string) error {
			return os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/empty\n"), 0o600)
		},
	} {
		root := t.TempDir()
		if err := setup(root); err != nil {
			t.Fatal(err)
		}
		if err := requireNativeAgentGoProfile(root); !errors.Is(err, ErrVerifierProfileUnavailable) {
			t.Fatalf("profile without a Go module/source was accepted: %v", err)
		}
	}
}

func TestGoVerifierRejectsMixedLanguageBeforeOpeningResourceScope(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent.py"), []byte("# unprofiled source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	limiter := &countingVerifierResourceLimiter{}
	verifier := GoTestNativeAgentVerifier{CommandUID: 10001, CommandGID: 10001, MemoryMB: 256, CPUMillis: 250,
		ResourceLimits: limiter}
	err := verifier.RunTests(context.Background(), ExecutionPlan{Kind: "agent", RepositoryPath: root}, io.Discard)
	if !errors.Is(err, ErrVerifierProfileUnavailable) || limiter.calls != 0 {
		t.Fatalf("mixed-language tree result = (%v), resource scope opens=%d", err, limiter.calls)
	}
}
